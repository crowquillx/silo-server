package jellycompat

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
		{"incompatible converted codec", `"CodecProfiles":[{"Type":"Audio","Codec":"aac","Conditions":[{"Condition":"LessThanEqual","Property":"AudioChannels","Value":"1"}]}]`, true, true, http.StatusBadRequest, false},
		{"incompatible converted container", `"ContainerProfiles":[{"Type":"Audio","Container":"mp4","Conditions":[{"Condition":"LessThanEqual","Property":"AudioChannels","Value":"1"}]}]`, true, true, http.StatusBadRequest, false},
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
		want                                 int
	}{
		{"m4a source with mp4 profile", "m4a", "mp4", `{}`, false, http.StatusOK},
		{"mp4 source with m4a profile", "mp4", "m4a", `{}`, false, http.StatusOK},
		{"m4b source with mp4 profile", "m4b", "mp4", `{}`, false, http.StatusOK},
		{"converted container condition", "mp3", "mp4", `{"ContainerProfiles":[{"Type":"Audio","Container":"m4a","Conditions":[{"Condition":"LessThanEqual","Property":"AudioChannels","Value":"1"}]}]}`, true, http.StatusBadRequest},
		{"converted codec condition", "mp3", "mp4", `{"CodecProfiles":[{"Type":"Audio","Container":"m4a","Codec":"aac","Conditions":[{"Condition":"LessThanEqual","Property":"AudioChannels","Value":"1"}]}]}`, true, http.StatusBadRequest},
		{"excluded codec container", "mp3", "mp4", `{"CodecProfiles":[{"Type":"Audio","Container":"-m4a,ogg","Codec":"aac","Conditions":[{"Condition":"LessThanEqual","Property":"AudioChannels","Value":"1"}]}]}`, true, http.StatusOK},
		{"unrelated container condition", "mp3", "mp4", `{"ContainerProfiles":[{"Type":"Audio","Container":"ogg","Conditions":[{"Condition":"LessThanEqual","Property":"AudioChannels","Value":"1"}]}]}`, true, http.StatusOK},
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
		})
	}
}
