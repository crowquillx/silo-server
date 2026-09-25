package jellycompat

import (
	"context"
	"encoding/binary"
	"errors"
	"math/rand/v2"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/netaccess"
	"github.com/Silo-Server/silo-server/internal/playback"
	"github.com/Silo-Server/silo-server/internal/streamtelemetry"
	"github.com/Silo-Server/silo-server/internal/themedelivery"
	"github.com/Silo-Server/silo-server/internal/themesongs"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

const (
	compatThemeAudioLower = "audio"
	compatThemeStream     = "stream"
	compatThemeAudio      = "Audio"
	compatThemeUniversal  = "universal"
	compatThemeM4A        = "m4a"
	compatThemeSeason     = "season"
	compatThemeAudioRate  = "audioBitRate"
	compatThemeMaxRate    = "maxStreamingBitrate"
	compatThemeFile       = "File"
	compatThemeHTTP       = "http"
)

type themeSongStore interface {
	themesongs.Store
	Find(context.Context, string, catalog.AccessFilter) (themesongs.File, error)
}

func (h *ItemsHandler) themeSongsResult(w http.ResponseWriter, r *http.Request) (themeMediaResultDTO, bool) {
	empty := themeMediaResultDTO{Items: []baseItemDTO{}, OwnerID: chi.URLParam(r, "id")}
	id, ok := h.validateThemeOwner(w, r)
	if !ok {
		return empty, false
	}
	if h.themeSongs == nil {
		return empty, true
	}
	session := SessionFromContext(r.Context())
	if userID := firstNonEmpty(chi.URLParam(r, "userId"), newCaseInsensitiveQuery(r.URL.Query()).Get("userId")); userID != "" && !validatePseudoUser(w, userID, session) {
		return empty, false
	}
	query := newCaseInsensitiveQuery(r.URL.Query())
	inherit := false
	var err error
	if value := query.Get("inheritFromParent"); value != "" {
		inherit, err = strconv.ParseBool(value)
		if err != nil {
			writeError(w, 400, "InvalidRequest", "Invalid inheritFromParent")
			return empty, false
		}
	}
	owner, files, err := h.themeSongs.Resolve(r.Context(), id, inherit, h.resolveAccessFilter(r.Context(), session))
	if err != nil {
		writeThemeLookupError(w, err)
		return empty, false
	}
	if strings.EqualFold(query.Get("sortBy"), "Random") {
		rand.Shuffle(len(files), func(i, j int) { files[i], files[j] = files[j], files[i] })
	}
	if owner != id {
		kind := EncodedIDItem
		if len(files) > 0 && files[0].OwnerType == compatThemeSeason {
			kind = EncodedIDSeason
		}
		empty.OwnerID = h.codec.EncodeStringID(kind, owner)
	}
	for _, file := range files {
		empty.Items = append(empty.Items, h.themeSongItem(file))
	}
	empty.TotalRecordCount = len(empty.Items)
	return empty, true
}

func (h *ItemsHandler) themeSongItem(file themesongs.File) baseItemDTO {
	n, _ := themesongs.NumericID(file.ID)
	themeID := EncodeNumericID(EncodedIDThemeSong, uint64(n)).String()
	return baseItemDTO{ServerID: h.mapper.serverID, ID: themeID, Name: file.Title, Type: compatThemeAudio, MediaType: compatThemeAudio, Container: file.Container, RunTimeTicks: int64(file.DurationSeconds) * 10000000,
		MediaSources: []mediaSourceDTO{{ID: themeID, Name: file.Title, Type: compatSubtitleDefault, Container: file.Container, RunTimeTicks: int64(file.DurationSeconds) * 10000000, SupportsDirectPlay: true, SupportsDirectStream: false, SupportsTranscoding: false}}}
}

func (h *ItemsHandler) handleThemeItem(w http.ResponseWriter, r *http.Request, session *Session, themeID int64) {
	if userID := newCaseInsensitiveQuery(r.URL.Query()).Get("userId"); userID != "" && !validatePseudoUser(w, userID, session) {
		return
	}
	if h.themeSongs == nil {
		writeError(w, http.StatusServiceUnavailable, "Unavailable", "Theme audio unavailable")
		return
	}
	file, err := h.themeSongs.Find(r.Context(), strconv.FormatInt(themeID, 10), h.resolveAccessFilter(r.Context(), session))
	if err != nil {
		writeThemeLookupError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, h.themeSongItem(file))
}

func (h *ItemsHandler) HandleThemeSongs(w http.ResponseWriter, r *http.Request) {
	result, ok := h.themeSongsResult(w, r)
	if ok {
		writeJSON(w, http.StatusOK, result)
	}
}

func (h *ItemsHandler) HandleThemeAudio(w http.ResponseWriter, r *http.Request) {
	session := SessionFromContext(r.Context())
	if session == nil {
		writeError(w, 401, "Unauthorized", "Missing authentication token")
		return
	}
	if h.themeSongs == nil {
		writeError(w, 503, "Unavailable", "Theme audio unavailable")
		return
	}
	id, err := DecodeID(chi.URLParam(r, "itemId"))
	if err != nil || id.Type != EncodedIDThemeSong {
		writeError(w, 404, "NotFound", "Theme not found")
		return
	}
	file, err := h.themeSongs.Find(r.Context(), strconv.FormatUint(id.Value, 10), h.resolveAccessFilter(r.Context(), session))
	if err != nil {
		writeThemeLookupError(w, err)
		return
	}
	query := newCaseInsensitiveQuery(r.URL.Query())
	universal := strings.EqualFold(path.Base(r.URL.Path), compatThemeUniversal)
	routeContainer := chi.URLParam(r, "container")
	delivery, method := themesongs.DeliveryOriginal, playback.PlayDirect
	var conversion themesongs.Conversion
	seekSeconds := 0.0
	if !themeDirectPlayAllowed(query, routeContainer, file, universal) {
		var reason string
		var ok bool
		conversion, seekSeconds, reason, ok = themeConversionAllowed(query, routeContainer, file, universal)
		if ok && h.themeRouter != nil && !h.themeRouter.CanConvert(r.Context()) {
			ok, reason = false, "Theme audio conversion is unavailable on this server"
		}
		if !ok {
			writeError(w, 400, "PlaybackUnavailable", reason)
			return
		}
		delivery, method = themesongs.DeliveryConverted, playback.PlayRemux
	}
	streamtelemetry.Attach(r.Context(), streamtelemetry.Attachment{Subject: streamtelemetry.UserSubject(session.StreamAppUserID), ProfileID: session.ProfileID, PlayMethod: string(method)})
	if h.themeRouter != nil {
		expires, _ := themesongs.Expiry(time.Now(), time.Time{})
		result, err := h.themeRouter.Resolve(r.Context(), themedelivery.Request{
			File: file, Delivery: delivery, Conversion: conversion, SeekSeconds: seekSeconds,
			UserID: session.StreamAppUserID, ProfileID: session.ProfileID,
			AccessPath: netaccess.PathFromContext(r.Context()), ExpiresAt: expires,
		})
		if err != nil {
			code := compatRoutingPolicyUnsatisfiedCode
			if errors.Is(err, themedelivery.ErrCapacityUnavailable) {
				code = compatRouteCapacityUnavailableCode
			}
			writeError(w, http.StatusServiceUnavailable, code, "No theme audio route satisfies the configured policy and current node availability")
			return
		}
		if !result.Local() {
			// Like compatibility video, a routed theme is a redirect to the proxy
			// the route reserved. A HEAD probe does not hold that capacity.
			http.Redirect(w, r, result.URL, http.StatusTemporaryRedirect)
			if r.Method == http.MethodHead {
				result.Release()
			}
			return
		}
	}

	f, err := themesongs.Open(file)
	if err != nil {
		writeError(w, 503, "PlaybackUnavailable", "Theme audio is unavailable on this node")
		return
	}
	defer func() { _ = f.Close() }()
	if delivery == themesongs.DeliveryConverted {
		ffmpeg := ""
		if h.themeFFmpegPath != nil {
			ffmpeg = h.themeFFmpegPath()
		}
		themesongs.ServeConverted(w, r, file.Path, conversion, seekSeconds, ffmpeg)
		return
	}
	themesongs.Serve(w, r, file, f)
}

// themeConversionAllowed decides whether a request the original cannot satisfy
// accepts the progressive AAC conversion, and with which output. Themes have
// no HLS transcode, and a static request asks for the original bytes only.
func themeConversionAllowed(query caseInsensitiveQuery, routeContainer string, file themesongs.File, universal bool) (themesongs.Conversion, float64, string, bool) {
	const unsupported = "Only original theme audio or its AAC conversion is supported"
	if strings.EqualFold(query.Get("static"), "true") {
		return themesongs.Conversion{}, 0, "Static theme streams serve original audio only", false
	}
	if stream := query.Get("audioStreamIndex"); stream != "" && stream != "-1" && stream != "0" {
		return themesongs.Conversion{}, 0, unsupported, false
	}
	containers := []string{routeContainer, query.Get("container")}
	if universal {
		if strings.EqualFold(query.Get("transcodingProtocol"), "hls") {
			return themesongs.Conversion{}, 0, "HLS theme transcoding is unsupported", false
		}
		// Universal Container lists direct-play formats; the conversion is
		// described by the transcoding parameters instead.
		containers = []string{query.Get("transcodingContainer")}
	}
	// The request must name an MP4 target itself: a client that did not ask for
	// a container could not expect the conversion's audio-only MP4.
	named := false
	for _, container := range containers {
		switch strings.ToLower(strings.TrimSpace(container)) {
		case "":
		case compatContainerMP4, compatThemeM4A:
			named = true
		default:
			return themesongs.Conversion{}, 0, unsupported, false
		}
	}
	if !named {
		return themesongs.Conversion{}, 0, unsupported, false
	}
	if codec := strings.ToLower(query.Get("audioCodec")); codec != "" && !containsThemeContainer(codec, themesongs.CodecAAC) {
		return themesongs.Conversion{}, 0, unsupported, false
	}
	target := file
	if query.Get("maxAudioChannels") == "1" || query.Get("transcodingAudioChannels") == "1" {
		target.AudioChannels = 1
	}
	conversion := themesongs.ConversionFor(target)
	if target.AudioChannels == 1 {
		conversion.SourceChannels = 0
	}
	for _, key := range []string{"maxAudioBitRate", compatThemeAudioRate, compatThemeMaxRate} {
		if value := query.Get(key); value != "" {
			bps, err := strconv.Atoi(value)
			if err != nil || bps <= 0 {
				return themesongs.Conversion{}, 0, unsupported, false
			}
			conversion.BitrateKbps = min(conversion.BitrateKbps, bps/1000)
		}
	}
	if conversion.BitrateKbps < 32 {
		return themesongs.Conversion{}, 0, "The requested bitrate is below the theme conversion minimum", false
	}
	seekSeconds := 0.0
	if raw := query.Get("startTimeTicks"); raw != "" && raw != "0" {
		ticks, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || ticks < 0 {
			return themesongs.Conversion{}, 0, unsupported, false
		}
		seekSeconds = float64(ticks) / 1e7
		if duration := float64(file.DurationSeconds); duration > 0 && seekSeconds > duration {
			seekSeconds = duration
		}
	}
	return conversion, seekSeconds, "", true
}

// decodeThemePlaybackID requires the complete numeric layout. An ordinary
// item's opaque UUID hash can share the type byte but not the reserved zeros.
func decodeThemePlaybackID(raw string) (int64, bool) {
	id, err := uuid.Parse(raw)
	if err != nil {
		return 0, false
	}
	value := binary.BigEndian.Uint64(id[8:])
	return int64(value), id == EncodeNumericID(EncodedIDThemeSong, value)
}

// handleThemePlaybackInfo negotiates the original or progressive AAC delivery
// supported by HandleThemeAudio. Theme songs have no native playback session.
func (h *PlaybackHandler) handleThemePlaybackInfo(w http.ResponseWriter, r *http.Request, session *Session, themeID int64) {
	if userID := chi.URLParam(r, "userId"); userID != "" && !validatePseudoUser(w, userID, session) {
		return
	}
	if h.themeSongs == nil {
		writeError(w, http.StatusServiceUnavailable, "Unavailable", "Theme audio unavailable")
		return
	}
	filter := withCompatAccessExclusions(catalog.AccessFilter{})
	if h.accessFilter != nil {
		filter = withCompatAccessExclusions(h.accessFilter(r.Context(), session.StreamAppUserID, session.ProfileID))
	}
	file, err := h.themeSongs.Find(r.Context(), strconv.FormatInt(themeID, 10), filter)
	if err != nil {
		writeThemeLookupError(w, err)
		return
	}
	req, profile, err := h.parsePlaybackRequest(r, session.Token)
	if err != nil {
		writeDeviceProfileRequestError(w, err, "Invalid playback request")
		return
	}
	serverCap, err := h.serverBitrateCap(r.Context(), session)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "PlaybackUnavailable", "The server could not resolve the stream bitrate limit")
		return
	}
	constraints := url.Values{}
	if !boolDefault(req.EnableDirectPlay, true) {
		constraints.Set("enableDirectPlay", "false")
	}
	// PlaybackInfo advertises one default track, index 0. Direct audio seeks
	// through byte ranges; only a conversion URL needs a server-side time seek.
	if req.StartTimeTicks < 0 || (req.AudioStreamIndex != nil && *req.AudioStreamIndex != -1 && *req.AudioStreamIndex != 0) {
		writeError(w, http.StatusBadRequest, "PlaybackUnavailable", "Invalid theme audio track or start time")
		return
	}
	if req.MaxAudioChannels > 0 {
		constraints.Set("maxAudioChannels", strconv.Itoa(req.MaxAudioChannels))
	}
	maxBitrate := req.MaxStreamingBitrate
	for _, cap := range []int64{profile.MaxStreamingBitrate, int64(serverCap) * 1000} {
		if cap > 0 && (maxBitrate <= 0 || cap < maxBitrate) {
			maxBitrate = cap
		}
	}
	if maxBitrate > 0 {
		constraints.Set("maxStreamingBitrate", strconv.FormatInt(maxBitrate, 10))
	}
	id := EncodeNumericID(EncodedIDThemeSong, uint64(themeID)).String()
	direct := themeDirectPlayAllowed(newCaseInsensitiveQuery(constraints), "", file, false) && themeAudioProfileAllows(profile, file)
	container, codec, channels, bitrate := strings.ToLower(file.Container), file.AudioCodec, file.AudioChannels, file.BitrateKbps
	streamURL := "/Audio/" + id + "/stream." + url.PathEscape(container)
	if direct {
		constraints.Set("static", "true")
	} else {
		if !boolDefault(req.EnableTranscoding, true) || h.themeCanConvert == nil || !h.themeCanConvert(r.Context()) {
			writeError(w, http.StatusBadRequest, "PlaybackUnavailable", "The theme cannot be played with the requested audio constraints")
			return
		}
		// Name the progressive MP4 target and force conversion even when the
		// source is already MP4 but direct play was disabled by the client.
		constraints.Set("enableDirectPlay", "false")
		constraints.Set("audioCodec", themesongs.CodecAAC)
		if req.StartTimeTicks > 0 {
			constraints.Set("startTimeTicks", strconv.FormatInt(req.StartTimeTicks, 10))
		}
		conversion, selected, ok := themePlaybackConversion(profile, file, constraints)
		if !ok {
			writeError(w, http.StatusBadRequest, "PlaybackUnavailable", "The converted theme does not meet the client's audio profile")
			return
		}
		constraints = selected
		container, codec, channels, bitrate = compatContainerMP4, themesongs.CodecAAC, conversion.Channels, conversion.BitrateKbps
		streamURL = "/Audio/" + id + "/stream.mp4"
	}
	constraints.Set("api_key", session.Token)
	streamURL += "?" + constraints.Encode()
	audioIndex := 0
	source := mediaSourceDTO{
		Protocol: compatThemeFile, ID: id, Type: compatSubtitleDefault, Container: container,
		Name: file.Title, RunTimeTicks: int64(file.DurationSeconds) * 10000000,
		SupportsDirectPlay: direct, SupportsTranscoding: !direct, SupportsProbing: true,
		Formats: []string{container}, RequiredHTTPHeaders: map[string]string{}, MediaAttachments: []map[string]any{},
		Bitrate: bitrate * 1000, DefaultAudioStreamIndex: &audioIndex,
		MediaStreams: []mediaStreamDTO{{Index: 0, Type: compatThemeAudio, Codec: codec, Channels: channels, BitRate: bitrate * 1000, IsDefault: true}},
	}
	if direct {
		source.Size = file.Size
		source.DirectStreamURL = streamURL
	} else {
		source.TranscodingContainer = container
		source.TranscodingSubProtocol = compatThemeHTTP
		source.TranscodingURL = streamURL
	}
	writeJSON(w, http.StatusOK, playbackInfoResponseDTO{MediaSources: []mediaSourceDTO{source}})
}

func themeAudioProfileAllows(profile DeviceProfile, file themesongs.File) bool {
	if !themeAudioConditionsAllow(profile, file) {
		return false
	}
	for _, direct := range profile.DirectPlayProfiles {
		if !themeAudioProfileType(direct.Type) {
			continue
		}
		if themeProfileContainerMatches(direct.Container, file.Container) && matchesCSV(direct.AudioCodec, file.AudioCodec) {
			return true
		}
	}
	// The generic profile and some clients only advertise video capabilities.
	// Their theme audio still uses the original-only stream route.
	return !slices.ContainsFunc(profile.DirectPlayProfiles, func(p DirectPlayProfile) bool {
		return themeAudioProfileType(p.Type)
	}) && !slices.ContainsFunc(profile.TranscodingProfiles, func(p TranscodingProfile) bool {
		return themeAudioProfileType(p.Type)
	})
}

// Jellyfin's enum default for a missing profile Type is Audio.
func themeAudioProfileType(kind string) bool {
	return kind == "" || strings.EqualFold(kind, compatThemeAudio)
}

// themePlaybackConversion tries the supported channel layouts and bitrate
// boundaries, then checks every condition against the actual output. This also
// handles ApplyConditions that change when the output bitrate changes.
func themePlaybackConversion(profile DeviceProfile, file themesongs.File, constraints url.Values) (themesongs.Conversion, url.Values, bool) {
	rates := themeProfileBitrates(profile)
	for _, channels := range []int{2, 1} {
		selected := url.Values{}
		for key, values := range constraints {
			selected[key] = slices.Clone(values)
		}
		if channels == 1 {
			selected.Set("maxAudioChannels", "1")
		}
		conversion, _, _, ok := themeConversionAllowed(newCaseInsensitiveQuery(selected), compatContainerMP4, file, false)
		if !ok {
			continue
		}
		candidates := append([]int{conversion.BitrateKbps}, rates...)
		for _, bitrate := range candidates {
			if bitrate > conversion.BitrateKbps {
				continue
			}
			output := themesongs.File{
				Song: themesongs.Song{Container: compatContainerMP4}, AudioCodec: themesongs.CodecAAC,
				AudioChannels: conversion.Channels, BitrateKbps: bitrate,
			}
			if !themeAACProfileAllows(profile, output) {
				continue
			}
			conversion.BitrateKbps = bitrate
			selected.Set("maxAudioChannels", strconv.Itoa(conversion.Channels))
			selected.Set(compatThemeAudioRate, strconv.Itoa(bitrate*1000))
			return conversion, selected, true
		}
	}
	return themesongs.Conversion{}, nil, false
}

// The encoder accepts whole kbps between 32 and 192. Each numeric condition
// can change truth only at its boundary; include adjacent kbps for strict
// inequalities and exclusions, then prefer the highest accepted bitrate.
func themeProfileBitrates(profile DeviceProfile) []int {
	rates := map[int]bool{}
	add := func(conditions []ProfileCondition) {
		for _, condition := range conditions {
			if normalizeConditionToken(condition.Property) != "audiobitrate" {
				continue
			}
			for _, value := range splitConditionSet(condition.Value) {
				n, err := strconv.Atoi(strings.TrimSpace(value))
				if err != nil {
					continue
				}
				for _, rate := range []int{n/1000 - 1, n / 1000, n/1000 + 1} {
					if rate >= 32 && rate <= 192 {
						rates[rate] = true
					}
				}
			}
		}
	}
	for _, p := range profile.TranscodingProfiles {
		add(p.Conditions)
	}
	for _, p := range profile.ContainerProfiles {
		add(p.Conditions)
	}
	for _, p := range profile.CodecProfiles {
		add(p.Conditions)
		add(p.ApplyConditions)
	}
	result := make([]int, 0, len(rates))
	for rate := range rates {
		result = append(result, rate)
	}
	slices.Sort(result)
	slices.Reverse(result)
	return result
}

func themeAACProfileAllows(profile DeviceProfile, output themesongs.File) bool {
	if !themeAudioConditionsAllow(profile, output) {
		return false
	}
	for _, conversion := range profile.TranscodingProfiles {
		if !themeAudioProfileType(conversion.Type) ||
			(conversion.Protocol != "" && !strings.EqualFold(conversion.Protocol, compatThemeHTTP)) ||
			(conversion.Context != "" && !strings.EqualFold(conversion.Context, "Streaming")) ||
			(conversion.Container == "" || !themeProfileContainerMatches(conversion.Container, compatContainerMP4)) ||
			!matchesCSV(conversion.AudioCodec, themesongs.CodecAAC) {
			continue
		}
		if conversion.MaxAudioChannels != "" {
			maxChannels, err := strconv.Atoi(conversion.MaxAudioChannels)
			if err != nil || maxChannels <= 0 || output.AudioChannels > maxChannels {
				continue
			}
		}
		if !conditionsMatch(conversion.Conditions, themeAudioConditionValues(output)) {
			continue
		}
		return true
	}
	return false
}

// Theme metadata contains only these audio facts. In particular, conversion
// does not promise the source's sample rate or codec profile. Missing values
// use the shared evaluator's required/optional condition semantics.
func themeAudioConditionValues(file themesongs.File) conditionValues {
	values := conditionValues{}
	for property, value := range map[string]int{
		"audiochannels":   file.AudioChannels,
		"audiobitrate":    file.BitrateKbps * 1000,
		"audiosamplerate": file.SampleRate,
	} {
		if fact := intConditionValue(value); fact.hasNum {
			values[property] = fact
		}
	}
	return values
}

func themeAudioConditionsAllow(profile DeviceProfile, file themesongs.File) bool {
	values := themeAudioConditionValues(file)
	for _, container := range profile.ContainerProfiles {
		audio := container.Type == "" || container.Type == "*" || strings.EqualFold(container.Type, compatThemeAudio)
		if audio && themeProfileContainerMatches(container.Container, file.Container) && !conditionsMatch(container.Conditions, values) {
			return false
		}
	}
	version := catalog.FileVersion{Container: file.Container, CodecAudio: file.AudioCodec}
	for _, codec := range profile.CodecProfiles {
		container := file.Container
		// A negative list excludes literal containers, not their entire alias family.
		if !strings.HasPrefix(strings.TrimSpace(codec.Container), "-") {
			container = themeProfileContainers(container)
		}
		if strings.EqualFold(codec.Type, compatThemeAudio) && codecProfileApplies(codec, version, nil, container, false) &&
			conditionsMatch(codec.ApplyConditions, values) && !conditionsMatch(codec.Conditions, values) {
			return false
		}
	}
	return true
}

func themeProfileContainerMatches(profile, container string) bool {
	if strings.HasPrefix(strings.TrimSpace(profile), "-") {
		return matchesCodecProfileContainer(profile, container)
	}
	for alias := range strings.SplitSeq(themeProfileContainers(container), ",") {
		if matchesCSV(profile, alias) {
			return true
		}
	}
	return false
}

// Theme delivery accepts MP4 audio extensions interchangeably in positive lists.
func themeProfileContainers(container string) string {
	if themeContainerFormat(strings.ToLower(strings.TrimSpace(container))) == compatContainerMP4 {
		return "mp4,m4a,m4b"
	}
	return container
}

func writeThemeLookupError(w http.ResponseWriter, err error) {
	if errors.Is(err, themesongs.ErrNotFound) || errors.Is(err, catalog.ErrItemNotFound) {
		writeError(w, http.StatusNotFound, "NotFound", "Theme not found")
		return
	}
	writeError(w, http.StatusInternalServerError, "InternalServerError", "Theme lookup failed")
}

func containsThemeContainer(list, container string) bool {
	for _, value := range strings.Split(list, ",") {
		if strings.TrimSpace(value) == container {
			return true
		}
	}
	return false
}

func themeContainerFormat(container string) string {
	if container == compatThemeM4A || container == "m4b" {
		return compatContainerMP4
	}
	return container
}

// Universal requests describe accepted formats and fallback transcode options.
// A fallback hint alone does not require conversion when the original fits.
func themeDirectPlayAllowed(query caseInsensitiveQuery, routeContainer string, file themesongs.File, universal bool) bool {
	for _, container := range []string{routeContainer, query.Get("container")} {
		if container != "" {
			accepted := false
			for _, value := range strings.Split(strings.ToLower(container), ",") {
				format, codecs, qualified := strings.Cut(strings.TrimSpace(value), "|")
				codecAccepted := !qualified || containsThemeContainer(strings.ReplaceAll(codecs, "|", ","), file.AudioCodec)
				accepted = accepted || themeContainerFormat(format) == themeContainerFormat(file.Container) && codecAccepted
			}
			if !accepted {
				return false
			}
		}
	}
	// Themes expose one audio stream. Both -1 and the advertised index 0 select it.
	if stream := query.Get("audioStreamIndex"); stream != "" && stream != "-1" && stream != "0" {
		return false
	}
	if strings.EqualFold(query.Get("static"), "false") || strings.EqualFold(query.Get("enableDirectPlay"), "false") {
		return false
	}
	// Universal AudioCodec selects the fallback encoder. Its Container list
	// separately declares direct-play formats, including container|codec entries.
	fallbackCodec := universal && query.Get("container") != ""
	if codec := query.Get("audioCodec"); codec != "" && !fallbackCodec && !containsThemeContainer(strings.ToLower(codec), file.AudioCodec) {
		return false
	}
	for _, limit := range []struct {
		key    string
		actual int
		exact  bool
	}{
		{compatThemeAudioRate, file.BitrateKbps * 1000, false}, {"maxAudioBitRate", file.BitrateKbps * 1000, false},
		{compatThemeMaxRate, file.BitrateKbps * 1000, false}, {"audioChannels", file.AudioChannels, true},
		{"maxAudioChannels", file.AudioChannels, false}, {"audioSampleRate", file.SampleRate, true},
		{"maxAudioSampleRate", file.SampleRate, false},
	} {
		if universal && limit.key == compatThemeAudioRate {
			// The universal route uses this only for its fallback encoder.
			continue
		}
		value := query.Get(limit.key)
		if value == "" {
			continue
		}
		if limit.actual <= 0 && limit.key == compatThemeMaxRate {
			// Jellyfin's StreamBuilder assumes 40 Mbps when bitrate is unknown.
			limit.actual = 40_000_000
		}
		n, err := strconv.Atoi(value)
		if err != nil || n <= 0 {
			return false
		}
		if universal && limit.actual <= 0 && (limit.key == "maxAudioChannels" || limit.key == "maxAudioSampleRate") {
			// These universal device-profile constraints are not required when
			// the source metadata is unknown.
			continue
		}
		if limit.actual <= 0 || (limit.exact && n != limit.actual) || (!limit.exact && n < limit.actual) {
			return false
		}
	}
	if start := query.Get("startTimeTicks"); start != "" && start != "0" {
		return false
	}
	return true
}
