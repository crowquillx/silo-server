package jellycompat

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/catalog"
	"github.com/Silo-Server/silo-server/internal/config"
	"github.com/Silo-Server/silo-server/internal/streamtelemetry"
	"github.com/Silo-Server/silo-server/internal/themedelivery"
	"github.com/Silo-Server/silo-server/internal/themesongs"
	"github.com/go-chi/chi/v5"
)

type compatThemeFixture struct {
	file    themesongs.File
	owner   string
	inherit bool
	err     error
}

type restrictedThemeFixture struct {
	compatThemeFixture
	lastFilter catalog.AccessFilter
}

func (s *restrictedThemeFixture) Find(ctx context.Context, id string, filter catalog.AccessFilter) (themesongs.File, error) {
	s.lastFilter = filter
	if len(filter.AllowedLibraryIDs) != 1 || filter.AllowedLibraryIDs[0] != 7 {
		return themesongs.File{}, themesongs.ErrNotFound
	}
	return s.compatThemeFixture.Find(ctx, id, filter)
}

func TestThemePlaybackInfoResolvesAuthorizedAudio(t *testing.T) {
	path := filepath.Join(t.TempDir(), "theme.ogg")
	if err := os.WriteFile(path, []byte("0123456789"), 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	store := &restrictedThemeFixture{compatThemeFixture: compatThemeFixture{file: themesongs.File{
		Song:       themesongs.Song{ID: "8", Title: "Theme", Container: "ogg", DurationSeconds: 6},
		AudioCodec: "vorbis", AudioChannels: 2, BitrateKbps: 128, OwnerPath: filepath.Dir(path), Path: path, Size: info.Size(), Modified: info.ModTime().Truncate(time.Microsecond),
	}}}
	codec := NewResourceIDCodec()
	playback := &PlaybackHandler{codec: codec, themeSongs: store, deviceProfiles: NewDeviceProfileStore(time.Hour, nil)}
	playback.accessFilter = func(_ context.Context, userID int, profileID string) catalog.AccessFilter {
		if userID == 4 && profileID == "allowed" {
			return catalog.AccessFilter{AllowedLibraryIDs: []int{7}, UserID: userID, ProfileID: profileID}
		}
		return catalog.AccessFilter{AllowedLibraryIDs: []int{}, UserID: userID, ProfileID: profileID}
	}
	items := &ItemsHandler{codec: codec, themeSongs: store, accessFilter: playback.accessFilter}
	sessions := NewSessionStore(time.Hour, nil)
	if err := sessions.Put(Session{Token: "test-token", StreamAppUserID: 4, ProfileID: "allowed"}); err != nil {
		t.Fatal(err)
	}
	router := chi.NewRouter()
	router.Get("/Items/{id}/PlaybackInfo", playback.HandlePlaybackInfo)
	router.Post("/Items/{id}/PlaybackInfo", playback.HandlePlaybackInfo)
	router.Get("/Users/{userId}/Items/{id}/PlaybackInfo", playback.HandlePlaybackInfo)
	router.Post("/Users/{userId}/Items/{id}/PlaybackInfo", playback.HandlePlaybackInfo)
	router.With(PlaybackSessionAuth(sessions, nil, nil)).Get("/Audio/{itemId}/stream.ogg", items.HandleThemeAudio)
	id := EncodeNumericID(EncodedIDThemeSong, 8).String()
	request := func(method, url, body, profile string, authenticated bool) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, url, strings.NewReader(body))
		if authenticated {
			r = r.WithContext(context.WithValue(r.Context(), compatSessionKey, &Session{Token: "test-token", StreamAppUserID: 4, ProfileID: profile, PseudoUserID: PseudoUserID(4, profile)}))
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}
	var streamURL string
	for _, prefix := range []string{"", "/Users/" + PseudoUserID(4, "allowed").String()} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			target := prefix + "/Items/" + id + "/PlaybackInfo"
			if method == http.MethodGet {
				target += "?AudioStreamIndex=0&StartTimeTicks=10000000"
			}
			w := request(method, target, `{"AudioStreamIndex":0,"StartTimeTicks":10000000}`, "allowed", true)
			if w.Code != http.StatusOK {
				t.Fatalf("%s PlaybackInfo status=%d body=%s", method, w.Code, w.Body.String())
			}
			var result playbackInfoResponseDTO
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if len(result.MediaSources) != 1 || result.MediaSources[0].ID != id || !result.MediaSources[0].SupportsDirectPlay || result.MediaSources[0].SupportsTranscoding {
				t.Fatalf("%s PlaybackInfo sources=%+v", method, result.MediaSources)
			}
			source := result.MediaSources[0]
			if source.Path != "" || source.Container != "ogg" || source.DefaultAudioStreamIndex == nil || *source.DefaultAudioStreamIndex != 0 || len(source.MediaStreams) != 1 || source.MediaStreams[0].Type != "Audio" || source.MediaStreams[0].Codec != "vorbis" {
				t.Fatalf("%s PlaybackInfo metadata=%+v", method, source)
			}
			streamURL = source.DirectStreamURL
			parsed, err := url.Parse(streamURL)
			if err != nil || parsed.Path != "/Audio/"+id+"/stream.ogg" || parsed.Query().Get("static") != "true" || parsed.Query().Get("api_key") != "test-token" || parsed.Query().Has("startTimeTicks") || parsed.Query().Has("audioStreamIndex") || result.PlaySessionID != "" {
				t.Fatalf("%s PlaybackInfo stream=%q session=%q", method, streamURL, result.PlaySessionID)
			}
		}
	}
	if store.lastFilter.UserID != 4 || store.lastFilter.ProfileID != "allowed" {
		t.Fatalf("access filter identity=%+v", store.lastFilter)
	}
	w := request(http.MethodGet, streamURL, "", "allowed", false)
	if w.Code != http.StatusOK || w.Body.String() != "0123456789" {
		t.Fatalf("stream status=%d body=%s", w.Code, w.Body.String())
	}
	ranged := httptest.NewRequest(http.MethodGet, streamURL+"&audioStreamIndex=0", nil)
	ranged.Header.Set("Range", "bytes=2-4")
	w = httptest.NewRecorder()
	router.ServeHTTP(w, ranged)
	if w.Code != http.StatusPartialContent || w.Body.String() != "234" {
		t.Fatalf("ranged stream status=%d body=%s", w.Code, w.Body.String())
	}
	w = request(http.MethodGet, "/Audio/"+id+"/stream.ogg", "", "allowed", false)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated stream status=%d body=%s", w.Code, w.Body.String())
	}
	for _, tc := range []struct {
		name, url, profile string
		authenticated      bool
		want               int
	}{
		{"unauthenticated", "/Items/" + id + "/PlaybackInfo", "allowed", false, http.StatusUnauthorized},
		{"hidden library", "/Items/" + id + "/PlaybackInfo", "denied", true, http.StatusNotFound},
		{"wrong user", "/Users/00000000-0000-0000-0000-000000000001/Items/" + id + "/PlaybackInfo", "allowed", true, http.StatusNotFound},
		{"unknown theme", "/Items/" + EncodeNumericID(EncodedIDThemeSong, 9).String() + "/PlaybackInfo", "allowed", true, http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := request(http.MethodGet, tc.url, "", tc.profile, tc.authenticated)
			if w.Code != tc.want {
				t.Fatalf("status=%d, want %d, body=%s", w.Code, tc.want, w.Body.String())
			}
		})
	}
	for _, tc := range []struct {
		name, body string
	}{
		{"direct play disabled", `{"EnableDirectPlay":false}`},
		{"over bitrate limit", `{"MaxStreamingBitrate":64000}`},
		{"over channel limit", `{"MaxAudioChannels":1}`},
		{"negative time", `{"StartTimeTicks":-1}`},
		{"nonexistent stream", `{"AudioStreamIndex":1}`},
		{"unsupported audio profile", `{"DeviceProfile":{"DirectPlayProfiles":[{"Type":"Audio","Container":"mp3"}]}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := request(http.MethodPost, "/Items/"+id+"/PlaybackInfo", tc.body, "allowed", true)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "PlaybackUnavailable") {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}

func TestThemePlaybackInfoConvertsThroughAdvertisedStream(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is not installed")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe is not installed")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "theme.ogg")
	generate := exec.CommandContext(t.Context(), ffmpeg, "-v", "error", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=6", "-ac", "2", "-c:a", "libvorbis", path)
	if out, err := generate.CombinedOutput(); err != nil {
		t.Fatalf("generate source: %v: %s", err, out)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	store := &compatThemeFixture{file: themesongs.File{
		Song:       themesongs.Song{ID: "8", Title: "Theme", Container: "ogg", DurationSeconds: 6},
		AudioCodec: "vorbis", AudioChannels: 2, BitrateKbps: 128,
		OwnerPath: dir, Path: path, Size: info.Size(), Modified: info.ModTime().Truncate(time.Microsecond),
	}}
	codec := NewResourceIDCodec()
	playback := &PlaybackHandler{codec: codec, themeSongs: store, deviceProfiles: NewDeviceProfileStore(time.Hour, nil)}
	items := &ItemsHandler{codec: codec, themeSongs: store, themeFFmpegPath: func() string { return ffmpeg }}
	items.themeRouter = &themedelivery.Router{LocalConversion: func(context.Context) bool { return true }}
	playback.themeCanConvert = items.themeRouter.CanRouteConversion
	sessions := NewSessionStore(time.Hour, nil)
	if err := sessions.Put(Session{Token: "test-token", StreamAppUserID: 4, ProfileID: "profile"}); err != nil {
		t.Fatal(err)
	}
	router := chi.NewRouter()
	router.Post("/Items/{id}/PlaybackInfo", playback.HandlePlaybackInfo)
	router.With(PlaybackSessionAuth(sessions, nil, nil)).Get("/Audio/{itemId}/stream.{container}", items.HandleThemeAudio)
	id := EncodeNumericID(EncodedIDThemeSong, 8).String()
	body := `{"MaxStreamingBitrate":96000,"StartTimeTicks":10000000,"DeviceProfile":{"TranscodingProfiles":[{"Type":"Audio","Protocol":"http","Container":"mp4","AudioCodec":"aac","MaxAudioChannels":"1"}]}}`
	request := httptest.NewRequest(http.MethodPost, "/Items/"+id+"/PlaybackInfo", strings.NewReader(body))
	request = request.WithContext(context.WithValue(request.Context(), compatSessionKey, &Session{Token: "test-token", StreamAppUserID: 4, ProfileID: "profile"}))
	w := httptest.NewRecorder()
	router.ServeHTTP(w, request)
	if w.Code != http.StatusOK {
		t.Fatalf("PlaybackInfo status=%d body=%s", w.Code, w.Body.String())
	}
	var result playbackInfoResponseDTO
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.MediaSources) != 1 {
		t.Fatalf("media sources=%d", len(result.MediaSources))
	}
	source := result.MediaSources[0]
	if source.SupportsDirectPlay || !source.SupportsTranscoding || source.Container != "mp4" || source.TranscodingSubProtocol != "http" || source.MediaStreams[0].Codec != "aac" || source.MediaStreams[0].Channels != 1 || source.Bitrate > 96000 {
		t.Fatalf("converted source=%+v", source)
	}
	parsed, err := url.Parse(source.TranscodingURL)
	if err != nil || parsed.Path != "/Audio/"+id+"/stream.mp4" || parsed.Query().Get("api_key") != "test-token" || parsed.Query().Get("enableDirectPlay") != "false" || parsed.Query().Get("startTimeTicks") != "10000000" {
		t.Fatalf("converted URL=%q err=%v", source.TranscodingURL, err)
	}
	w = httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, source.TranscodingURL, nil))
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != themesongs.ConvertedContentType {
		t.Fatalf("converted stream status=%d body=%q headers=%v", w.Code, w.Body.String(), w.Header())
	}
	output := filepath.Join(dir, "converted.m4a")
	if err := os.WriteFile(output, w.Body.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	probe := exec.CommandContext(t.Context(), ffprobe, "-v", "error", "-select_streams", "a", "-show_entries", "stream=channels", "-of", "csv=p=0", output)
	if out, err := probe.CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != "1" {
		t.Fatalf("converted channels: %v: %s", err, out)
	}
	decode := exec.CommandContext(t.Context(), ffmpeg, "-v", "error", "-xerror", "-i", output, "-f", "null", "-")
	if out, err := decode.CombinedOutput(); err != nil {
		t.Fatalf("decode converted theme: %v: %s", err, out)
	}
	// A policy change prevents new offers and preserves the existing stream's
	// routing-error response, rather than reporting an unsupported client codec.
	policy := config.DefaultPlaybackRoutingPolicy()
	policy.RemuxExecution = config.PlaybackExecutionWorkerOnly
	items.themeRouter.Policy = func() config.PlaybackRoutingPolicy { return policy }
	denied := httptest.NewRequest(http.MethodPost, "/Items/"+id+"/PlaybackInfo", strings.NewReader(body))
	denied = denied.WithContext(request.Context())
	w = httptest.NewRecorder()
	router.ServeHTTP(w, denied)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("unroutable conversion offered: %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, source.TranscodingURL, nil))
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), compatRoutingPolicyUnsatisfiedCode) {
		t.Fatalf("routing error changed: %d %s", w.Code, w.Body.String())
	}
}

func TestThemePlaybackReportsDoNotCreateSessions(t *testing.T) {
	mgr := &testCompatSessionManager{}
	h, _ := newActiveEncodingsHandler(mgr)
	syncer := &recordingSessionSyncer{}
	h.SessionSyncer = syncer
	for _, handle := range []http.HandlerFunc{h.HandleSessionPlaying, h.HandleSessionPlayingProgress, h.HandleSessionPlayingStopped} {
		body := strings.NewReader(`{"ItemId":"0b000000-0000-0000-0000-000000000007","MediaSourceId":"0b000000-0000-0000-0000-000000000007","PlaySessionId":"theme-session","PositionTicks":50000000}`)
		req := withCompatSession(httptest.NewRequest("POST", "/Sessions/Playing", body), "tok")
		rec := httptest.NewRecorder()
		handle(rec, req)
		if rec.Code != 204 || syncer.calls != 0 || len(mgr.sessions) != 0 || mgr.progressCalls != 0 || len(mgr.stopCalls) != 0 {
			t.Fatalf("theme report changed playback state: status=%d syncs=%d sessions=%d", rec.Code, syncer.calls, len(mgr.sessions))
		}
	}
}

func TestThemeDirectPlayFormats(t *testing.T) {
	file := themesongs.File{Song: themesongs.Song{Container: "m4a"}, AudioCodec: "aac", AudioChannels: 2, BitrateKbps: 128, SampleRate: 44100}
	for _, tc := range []struct {
		query     string
		container string
		allowed   bool
	}{
		{"container=mp4,mp3&audioCodec=aac&transcodingContainer=ts&transcodingProtocol=hls", "", true},
		{"", "mp4", true},
		{"maxAudioBitRate=128000&maxAudioChannels=2&audioSampleRate=44100&audioStreamIndex=-1", "m4a", true},
		{"audioCodec=mp3", "", false},
		{"audioStreamIndex=1", "", false},
		{"audioStreamIndex=0", "", true},
		{"maxAudioBitRate=64000", "", false},
		{"audioChannels=1", "", false},
		{"audioSampleRate=48000", "", false},
		{"container=mp4", "ogg", false},
	} {
		t.Run(tc.container+"?"+tc.query, func(t *testing.T) {
			values, err := url.ParseQuery(tc.query)
			if err != nil {
				t.Fatal(err)
			}
			if got := themeDirectPlayAllowed(newCaseInsensitiveQuery(values), tc.container, file, false); got != tc.allowed {
				t.Fatalf("allowed=%v, want %v", got, tc.allowed)
			}
		})
	}
}

func TestThemeUniversalFallbackNegotiation(t *testing.T) {
	file := themesongs.File{Song: themesongs.Song{Container: "mp3"}, AudioCodec: "mp3", AudioChannels: 2, BitrateKbps: 128, SampleRate: 44100}
	for _, tc := range []struct {
		query     string
		universal bool
		allowed   bool
	}{
		{"container=mp3&audioCodec=aac&transcodingContainer=mp4&transcodingProtocol=hls", true, true},
		{"container=mp3&audioCodec=aac&transcodingContainer=mp4&transcodingProtocol=hls", false, false},
		{"container=mp3|mp3&audioCodec=aac", true, true},
		{"container=mp3|aac&audioCodec=mp3", true, false},
		{"container=aac&audioCodec=mp3", true, false},
		{"audioCodec=aac&transcodingContainer=mp4", true, false},
		{"container=mp3&audioCodec=aac&maxStreamingBitrate=64000", true, false},
		{"container=mp3&audioCodec=aac&maxAudioChannels=1", true, false},
		{"container=mp3&audioCodec=aac&maxAudioSampleRate=22050", true, false},
		{"container=mp3&audioCodec=aac&audioBitRate=64000&transcodingAudioChannels=1", true, true},
	} {
		t.Run(tc.query, func(t *testing.T) {
			values, err := url.ParseQuery(tc.query)
			if err != nil {
				t.Fatal(err)
			}
			if got := themeDirectPlayAllowed(newCaseInsensitiveQuery(values), "", file, tc.universal); got != tc.allowed {
				t.Fatalf("allowed=%v, want %v", got, tc.allowed)
			}
		})
	}
}

func TestThemeUniversalUnknownMetadata(t *testing.T) {
	file := themesongs.File{Song: themesongs.Song{Container: "mp3"}, AudioCodec: "mp3"}
	for _, tc := range []struct {
		query   string
		allowed bool
	}{
		{"Container=mp3&MaxStreamingBitrate=40000000&MaxAudioChannels=2&MaxAudioSampleRate=44100", true},
		{"Container=mp3&MaxStreamingBitrate=39999999", false},
		{"Container=mp3&MaxAudioChannels=invalid", false},
	} {
		values, err := url.ParseQuery(tc.query)
		if err != nil {
			t.Fatal(err)
		}
		if got := themeDirectPlayAllowed(newCaseInsensitiveQuery(values), "", file, true); got != tc.allowed {
			t.Fatalf("%s: allowed=%v, want %v", tc.query, got, tc.allowed)
		}
	}
}

func (s *compatThemeFixture) Resolve(_ context.Context, id string, inherit bool, _ catalog.AccessFilter) (string, []themesongs.File, error) {
	s.inherit = inherit
	if inherit && s.owner != "" {
		return s.owner, []themesongs.File{s.file}, s.err
	}
	return id, []themesongs.File{s.file}, s.err
}
func (s *compatThemeFixture) Find(_ context.Context, id string, _ catalog.AccessFilter) (themesongs.File, error) {
	if s.err != nil {
		return themesongs.File{}, s.err
	}
	if id != s.file.ID {
		return themesongs.File{}, themesongs.ErrNotFound
	}
	return s.file, nil
}

func TestCompatThemeDiscoveryAndAudio(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "theme.mp3")
	if err := os.WriteFile(path, []byte("0123456789"), 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	store := &compatThemeFixture{file: themesongs.File{Song: themesongs.Song{ID: "7", Title: "Theme", Container: "mp3"}, AudioCodec: "mp3", OwnerPath: dir, Path: path, Size: info.Size(), Modified: info.ModTime().Truncate(time.Microsecond)}}
	codec := NewResourceIDCodec()
	h := &ItemsHandler{codec: codec, mapper: &mapper{serverID: "theme-server"}, content: &countingContentService{}, themeSongs: store}
	router := chi.NewRouter()
	router.Get("/Items/{id}", h.HandleItem)
	router.Get("/Users/{userId}/Items/{id}", h.HandleItem)
	router.Get("/Items/{id}/ThemeSongs", h.HandleThemeSongs)
	router.Get("/Items/{id}/ThemeMedia", h.HandleThemeMedia)
	cfg := streamtelemetry.DefaultConfig("theme-test")
	cfg.Enabled = true
	registry := streamtelemetry.NewRegistry(cfg, streamtelemetry.NewLocalStore(), nil)
	for _, path := range []string{"/Audio/{itemId}/stream", "/Audio/{itemId}/stream.{container}", "/Audio/{itemId}/universal"} {
		router.Get(path, observeCompat(registry, "GET", path, h.HandleThemeAudio))
		router.Head(path, observeCompat(registry, "HEAD", path, h.HandleThemeAudio))
	}
	request := func(method, path, rangeValue string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		r.Header.Set("Range", rangeValue)
		r = r.WithContext(context.WithValue(r.Context(), compatSessionKey, &Session{StreamAppUserID: 1, ProfileID: "profile"}))
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}
	owner := codec.EncodeStringID(EncodedIDItem, "movie")
	w := request("GET", "/Items/"+owner+"/ThemeSongs?inheritFromParent=false&sortBy=Random", "")
	var result themeMediaResultDTO
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || store.inherit || len(result.Items) != 1 || result.OwnerID != owner || result.TotalRecordCount != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
	if result.Items[0].ServerID != "theme-server" {
		t.Fatal("Jellyfin Web needs the theme's server identity to select its API client")
	}
	w = request("GET", "/Items/"+owner+"/ThemeMedia", "")
	if store.inherit {
		t.Fatal("omitted inheritFromParent must default to false")
	}
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"ThemeVideosResult":{"Items":[]`) {
		t.Fatal(w.Code, w.Body.String())
	}
	store.owner, store.file.OwnerType = "series-S01", "season"
	w = request("GET", "/Items/"+owner+"/ThemeSongs?inheritFromParent=true", "")
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || result.OwnerID != codec.EncodeStringID(EncodedIDSeason, "series-S01") {
		t.Fatal("synthetic season owner lost its season ID", w.Code, w.Body.String())
	}
	store.owner, store.file.OwnerType = "", ""
	id := EncodeNumericID(EncodedIDThemeSong, 7).String()
	// Jellyfin Web fetches the discovered theme's item detail before requesting
	// universal audio. Both item routes must preserve the discovery metadata.
	for _, prefix := range []string{"", "/Users/00000000-0000-0000-0000-000000000000"} {
		w = request("GET", prefix+"/Items/"+id, "")
		var item baseItemDTO
		if err := json.Unmarshal(w.Body.Bytes(), &item); err != nil {
			t.Fatal(err)
		}
		if w.Code != 200 || item.ServerID != "theme-server" || item.ID != id || item.MediaType != "Audio" || item.Container != "mp3" {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	// This is the negotiation Jellyfin Web 12.1 sends for an MP3 theme. The
	// AudioCodec describes the HLS fallback, while Container allows direct MP3.
	w = request("GET", "/Audio/"+id+"/universal?UserId=00000000-0000-0000-0000-000000000000&DeviceId=theme-test&MaxStreamingBitrate=697095436&Container=opus,webm%7Copus,ts%7Cmp3,mp3,aac,m4a%7Caac,m4b%7Caac,flac,webma,webm%7Cwebma,wav,ogg&TranscodingContainer=mp4&TranscodingProtocol=hls&AudioCodec=aac&PlaySessionId=theme-test-session&StartTimeTicks=0&EnableRedirection=true&EnableRemoteMedia=false&EnableAudioVbrEncoding=true", "bytes=2-4")
	if w.Code != 206 || w.Body.String() != "234" {
		t.Fatal(w.Code, w.Body.String())
	}
	// A fresh codec must still resolve the theme ID after a restart.
	h.codec = NewResourceIDCodec()
	for _, suffix := range []string{"stream", "stream.mp3", "universal"} {
		w = request("GET", "/Audio/"+id+"/"+suffix, "bytes=2-4")
		if w.Code != 206 || w.Body.String() != "234" {
			t.Fatal(w.Code, w.Body.String())
		}
		w = request("HEAD", "/Audio/"+id+"/"+suffix, "")
		if w.Code != 200 || w.Body.Len() != 0 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	for _, suffix := range []string{"stream.ogg", "universal?container=ogg", "stream?static=false", "universal?audioCodec=aac"} {
		w = request("GET", "/Audio/"+id+"/"+suffix, "")
		if w.Code != 400 || !strings.Contains(w.Body.String(), "PlaybackUnavailable") {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	snapshot := registry.Sweep()
	if len(snapshot.Transfers) == 0 || len(snapshot.Sessions) != 0 {
		t.Fatalf("theme activity: %+v", snapshot)
	}
	for _, transfer := range snapshot.Transfers {
		if transfer.Subject != streamtelemetry.UserSubject(1) || transfer.ProfileID != "profile" {
			t.Fatalf("theme identity: %+v", transfer)
		}
	}
	h.codec = codec
	for _, tc := range []struct {
		err    error
		status int
	}{{errors.New("database failed"), 500}, {themesongs.ErrNotFound, 404}} {
		store.err = tc.err
		for _, path := range []string{"/Items/" + owner + "/ThemeSongs", "/Audio/" + id + "/stream"} {
			if got := request("GET", path, ""); got.Code != tc.status {
				t.Fatalf("lookup error status=%d want=%d", got.Code, tc.status)
			}
		}
	}
}
