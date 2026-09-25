package jellycompat

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/themesongs"
	"github.com/go-chi/chi/v5"
)

func TestThemePlaybackInfoHonorsAudioProfileConditions(t *testing.T) {
	const directFormat = `"DirectPlayProfiles":[{"Type":"Audio","Container":"mp3","AudioCodec":"mp3"}]`
	const conversionFormat = `"TranscodingProfiles":[{"Type":"Audio","Protocol":"http","Container":"mp4","AudioCodec":"aac"}]`
	for _, tc := range []struct {
		name, conditions string
		offersConversion bool
		forceConversion  bool
		want             int
		converted        bool
	}{
		{"codec channel mismatch", `"CodecProfiles":[{"Type":"Audio","Codec":"mp3","Conditions":[{"Condition":"LessThanEqual","Property":"AudioChannels","Value":"1"}]}]`, false, false, http.StatusBadRequest, false},
		{"container channel mismatch", `"ContainerProfiles":[{"Type":"Audio","Container":"mp3","Conditions":[{"Condition":"LessThanEqual","Property":"AudioChannels","Value":"1"}]}]`, false, false, http.StatusBadRequest, false},
		{"matching conditions", `"CodecProfiles":[{"Type":"Audio","Codec":"mp3","Conditions":[{"Condition":"LessThanEqual","Property":"AudioChannels","Value":"2"}]}]`, false, false, http.StatusOK, false},
		{"inapplicable conditions", `"CodecProfiles":[{"Type":"Audio","Codec":"mp3","ApplyConditions":[{"Condition":"GreaterThanEqual","Property":"AudioChannels","Value":"6"}],"Conditions":[{"Condition":"LessThanEqual","Property":"AudioChannels","Value":"1"}]}]`, false, false, http.StatusOK, false},
		{"required unknown source property", `"CodecProfiles":[{"Type":"Audio","Codec":"mp3","Conditions":[{"Condition":"Equals","Property":"AudioProfile","Value":"LC"}]}]`, false, false, http.StatusBadRequest, false},
		{"optional unknown source property", `"CodecProfiles":[{"Type":"Audio","Codec":"mp3","Conditions":[{"Condition":"Equals","Property":"AudioProfile","Value":"LC","IsRequired":false}]}]`, false, false, http.StatusOK, false},
		{"compatible conversion", `"CodecProfiles":[{"Type":"Audio","Codec":"mp3","Conditions":[{"Condition":"LessThanEqual","Property":"AudioChannels","Value":"1"}]}]`, true, false, http.StatusOK, true},
		{"converted codec requires mono", `"CodecProfiles":[{"Type":"Audio","Codec":"aac","Conditions":[{"Condition":"LessThanEqual","Property":"AudioChannels","Value":"1"}]}]`, true, true, http.StatusOK, true},
		{"converted container requires mono", `"ContainerProfiles":[{"Type":"Audio","Container":"mp4","Conditions":[{"Condition":"LessThanEqual","Property":"AudioChannels","Value":"1"}]}]`, true, true, http.StatusOK, true},
		{"required unknown converted property", `"CodecProfiles":[{"Type":"Audio","Codec":"aac","Conditions":[{"Condition":"LessThanEqual","Property":"AudioSampleRate","Value":"44100"}]}]`, true, true, http.StatusBadRequest, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &compatThemeFixture{file: themesongs.File{
				Song:       themesongs.Song{ID: "8", Container: "mp3", DurationSeconds: 6},
				AudioCodec: "mp3", AudioChannels: 2, BitrateKbps: 128, SampleRate: 44100,
			}}
			h := &PlaybackHandler{
				codec: NewResourceIDCodec(), themeSongs: store, deviceProfiles: NewDeviceProfileStore(time.Hour, nil),
				themeCanConvert: func(context.Context) bool { return true },
			}
			router := chi.NewRouter()
			router.Post("/Items/{id}/PlaybackInfo", h.HandlePlaybackInfo)
			profile := directFormat + "," + tc.conditions
			if tc.offersConversion {
				profile += "," + conversionFormat
			}
			body, err := json.Marshal(map[string]any{
				"EnableDirectPlay": !tc.forceConversion,
				"DeviceProfile":    json.RawMessage("{" + profile + "}"),
			})
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest(http.MethodPost, "/Items/"+EncodeNumericID(EncodedIDThemeSong, 8).String()+"/PlaybackInfo", bytes.NewReader(body))
			r = r.WithContext(context.WithValue(r.Context(), compatSessionKey, &Session{Token: "test-token"}))
			w := httptest.NewRecorder()
			router.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status=%d, want %d: %s", w.Code, tc.want, w.Body.String())
			}
			if tc.want != http.StatusOK {
				return
			}
			var result playbackInfoResponseDTO
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if len(result.MediaSources) != 1 || result.MediaSources[0].SupportsTranscoding != tc.converted || result.MediaSources[0].SupportsDirectPlay == tc.converted {
				t.Fatalf("unexpected sources: %+v", result.MediaSources)
			}
		})
	}
}

func TestThemePlaybackInfoMP4AudioAliases(t *testing.T) {
	for _, tc := range []struct {
		name, source, advertised, conditions string
		convert                              bool
		want, channels                       int
	}{
		{"m4a source with mp4 profile", "m4a", "mp4", `{}`, false, http.StatusOK, 2},
		{"mp4 source with m4a profile", "mp4", "m4a", `{}`, false, http.StatusOK, 2},
		{"m4b source with mp4 profile", "m4b", "mp4", `{}`, false, http.StatusOK, 2},
		{"converted container condition", "mp3", "mp4", `{"ContainerProfiles":[{"Type":"Audio","Container":"m4a","Conditions":[{"Condition":"LessThanEqual","Property":"AudioChannels","Value":"1"}]}]}`, true, http.StatusOK, 1},
		{"converted codec condition", "mp3", "mp4", `{"CodecProfiles":[{"Type":"Audio","Container":"m4a","Codec":"aac","Conditions":[{"Condition":"LessThanEqual","Property":"AudioChannels","Value":"1"}]}]}`, true, http.StatusOK, 1},
		{"excluded codec container", "mp3", "mp4", `{"CodecProfiles":[{"Type":"Audio","Container":"-m4a,ogg","Codec":"aac","Conditions":[{"Condition":"LessThanEqual","Property":"AudioChannels","Value":"1"}]}]}`, true, http.StatusOK, 1},
		{"unrelated container condition", "mp3", "mp4", `{"ContainerProfiles":[{"Type":"Audio","Container":"ogg","Conditions":[{"Condition":"LessThanEqual","Property":"AudioChannels","Value":"1"}]}]}`, true, http.StatusOK, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &compatThemeFixture{file: themesongs.File{
				Song:       themesongs.Song{ID: "8", Container: tc.source},
				AudioCodec: "aac", AudioChannels: 2, BitrateKbps: 128,
			}}
			if tc.convert {
				store.file.AudioCodec = "mp3"
			}
			h := &PlaybackHandler{
				codec: NewResourceIDCodec(), themeSongs: store, deviceProfiles: NewDeviceProfileStore(time.Hour, nil),
				themeCanConvert: func(context.Context) bool { return true },
			}
			var profile DeviceProfile
			if err := json.Unmarshal([]byte(tc.conditions), &profile); err != nil {
				t.Fatal(err)
			}
			profile.DirectPlayProfiles = []DirectPlayProfile{{Type: "Audio", Container: tc.advertised, AudioCodec: "aac"}}
			profile.TranscodingProfiles = []TranscodingProfile{{Type: "Audio", Protocol: "http", Container: "m4a", AudioCodec: "aac"}}
			body, err := json.Marshal(map[string]any{
				"EnableDirectPlay": !tc.convert, "EnableTranscoding": tc.convert, "DeviceProfile": profile,
			})
			if err != nil {
				t.Fatal(err)
			}
			router := chi.NewRouter()
			router.Post("/Items/{id}/PlaybackInfo", h.HandlePlaybackInfo)
			r := httptest.NewRequest(http.MethodPost, "/Items/"+EncodeNumericID(EncodedIDThemeSong, 8).String()+"/PlaybackInfo", bytes.NewReader(body))
			r = r.WithContext(context.WithValue(r.Context(), compatSessionKey, &Session{Token: "test-token"}))
			w := httptest.NewRecorder()
			router.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status=%d, want %d: %s", w.Code, tc.want, w.Body.String())
			}
			var response playbackInfoResponseDTO
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if len(response.MediaSources) != 1 || len(response.MediaSources[0].MediaStreams) != 1 || response.MediaSources[0].MediaStreams[0].Channels != tc.channels {
				t.Fatalf("unexpected alias output: %+v", response.MediaSources)
			}
		})
	}
}

func TestThemePlaybackInfoProfileDefaultsAndLimits(t *testing.T) {
	const aac = `{"Type":"Audio","Protocol":"http","Container":"mp4","AudioCodec":"aac"}`
	for _, tc := range []struct {
		name, profiles, conditions string
		want, channels, bitrate    int
	}{
		{"explicit defaults", aac, "", 200, 2, 192000},
		{"omitted defaults", `{"Container":"mp4","AudioCodec":"aac"}`, "", 200, 2, 192000},
		{"streaming context", `{"Container":"mp4","Context":"Streaming"}`, "", 200, 2, 192000},
		{"static context", `{"Type":"Audio","Protocol":"http","Container":"mp4","Context":"Static"}`, "", 400, 0, 0},
		{"HLS remains unsupported", `{"Container":"mp4","Protocol":"hls"}`, "", 400, 0, 0},
		{"profile mono", `{"Container":"mp4","MaxAudioChannels":"1"}`, "", 200, 1, 128000},
		{"invalid channel limit", `{"Container":"mp4","MaxAudioChannels":"invalid"}`, "", 400, 0, 0},
		{"later usable profile", `{"Container":"mp4","MaxAudioChannels":"invalid"},` + aac, "", 200, 2, 192000},
		{"profile bitrate ceiling", `{"Container":"mp4","Conditions":[{"Condition":"LessThanEqual","Property":"AudioBitrate","Value":"65000"}]}`, "", 200, 2, 65000},
		{"codec mono", aac, `,"CodecProfiles":[{"Type":"Audio","Codec":"aac","Conditions":[{"Condition":"LessThanEqual","Property":"AudioChannels","Value":"1"}]}]`, 200, 1, 128000},
		{"container bitrate", aac, `,"ContainerProfiles":[{"Type":"Audio","Container":"m4a","Conditions":[{"Condition":"LessThanEqual","Property":"AudioBitrate","Value":"64000"}]}]`, 200, 2, 64000},
		{"container exclusion requires mono", aac, `,"ContainerProfiles":[{"Type":"Audio","Container":"-ogg","Conditions":[{"Condition":"LessThanEqual","Property":"AudioChannels","Value":"1"}]}]`, 200, 1, 128000},
		{"strict bitrate ceiling", `{"Container":"mp4","Conditions":[{"Condition":"LessThan","Property":"AudioBitrate","Value":"64000"}]}`, "", 200, 2, 63000},
		{"bitrate set", `{"Container":"mp4","Conditions":[{"Condition":"EqualsAny","Property":"AudioBitrate","Value":"64000,96000"}]}`, "", 200, 2, 96000},
		{"pipe-separated bitrate set", `{"Container":"mp4","Conditions":[{"Condition":"EqualsAny","Property":"AudioBitrate","Value":"64000|96000"}]}`, "", 200, 2, 96000},
		{"bitrate-dependent condition", aac, `,"CodecProfiles":[{"Type":"Audio","Codec":"aac","ApplyConditions":[{"Condition":"GreaterThan","Property":"AudioBitrate","Value":"64000"}],"Conditions":[{"Condition":"LessThanEqual","Property":"AudioChannels","Value":"1"}]}]`, 200, 2, 64000},
		{"below encoder floor", aac, `,"CodecProfiles":[{"Type":"Audio","Codec":"aac","Conditions":[{"Condition":"LessThanEqual","Property":"AudioBitrate","Value":"31000"}]}]`, 400, 0, 0},
		{"literal MP4 exclusion", aac, `,"CodecProfiles":[{"Type":"Audio","Container":"-mp4","Codec":"aac","Conditions":[{"Condition":"LessThanEqual","Property":"AudioChannels","Value":"1"}]}]`, 200, 2, 192000},
		{"M4A exclusion still constrains MP4", aac, `,"CodecProfiles":[{"Type":"Audio","Container":"-m4a","Codec":"aac","Conditions":[{"Condition":"LessThanEqual","Property":"AudioChannels","Value":"1"}]}]`, 200, 1, 128000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := themesongs.File{Song: themesongs.Song{ID: "8", Container: "mp3"}, AudioCodec: "mp3", AudioChannels: 2, BitrateKbps: 256}
			h := &PlaybackHandler{codec: NewResourceIDCodec(), themeSongs: &compatThemeFixture{file: file}, deviceProfiles: NewDeviceProfileStore(time.Hour, nil), themeCanConvert: func(context.Context) bool { return true }}
			router := chi.NewRouter()
			router.Post("/Items/{id}/PlaybackInfo", h.HandlePlaybackInfo)
			body := `{"EnableDirectPlay":false,"DeviceProfile":{"TranscodingProfiles":[` + tc.profiles + `]` + tc.conditions + `}}`
			req := httptest.NewRequest(http.MethodPost, "/Items/"+EncodeNumericID(EncodedIDThemeSong, 8).String()+"/PlaybackInfo", strings.NewReader(body))
			req = req.WithContext(context.WithValue(req.Context(), compatSessionKey, &Session{Token: "test-token"}))
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Fatalf("status=%d want=%d: %s", w.Code, tc.want, w.Body.String())
			}
			if tc.want != http.StatusOK {
				return
			}
			var response playbackInfoResponseDTO
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			source := response.MediaSources[0]
			if source.SupportsDirectPlay || !source.SupportsTranscoding || source.Bitrate != tc.bitrate || source.MediaStreams[0].Channels != tc.channels {
				t.Fatalf("output=%+v", source)
			}
			u, err := url.Parse(source.TranscodingURL)
			if err != nil {
				t.Fatal(err)
			}
			actual, _, _, ok := themeConversionAllowed(newCaseInsensitiveQuery(u.Query()), "mp4", file, false)
			if !ok || actual.Channels != tc.channels || actual.BitrateKbps*1000 != tc.bitrate {
				t.Fatalf("advertised stream produces %+v, accepted=%v", actual, ok)
			}
		})
	}
}
