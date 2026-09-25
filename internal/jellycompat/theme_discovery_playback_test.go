package jellycompat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Silo-Server/silo-server/internal/themesongs"
	"github.com/go-chi/chi/v5"
)

func TestDiscoveredThemeSupportsPlaybackInfo(t *testing.T) {
	store := &compatThemeFixture{file: themesongs.File{
		Song:       themesongs.Song{ID: "7", Title: "Theme", Container: "mp3"},
		AudioCodec: "mp3", AudioChannels: 2, BitrateKbps: 192,
	}}
	codec := NewResourceIDCodec()
	items := &ItemsHandler{codec: codec, mapper: &mapper{serverID: "theme-server"}, content: &countingContentService{}, themeSongs: store}
	playback := &PlaybackHandler{codec: codec, themeSongs: store, deviceProfiles: NewDeviceProfileStore(time.Hour, nil)}
	router := chi.NewRouter()
	router.Get("/Items/{id}/ThemeSongs", items.HandleThemeSongs)
	router.Get("/Items/{id}/PlaybackInfo", playback.HandlePlaybackInfo)
	request := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r = r.WithContext(context.WithValue(r.Context(), compatSessionKey, &Session{Token: "test-token"}))
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}
	owner := codec.EncodeStringID(EncodedIDItem, "movie")
	discovery := request("/Items/" + owner + "/ThemeSongs")
	var themes themeMediaResultDTO
	if err := json.Unmarshal(discovery.Body.Bytes(), &themes); err != nil {
		t.Fatal(err)
	}
	if discovery.Code != http.StatusOK || len(themes.Items) != 1 {
		t.Fatalf("theme discovery: status=%d body=%s", discovery.Code, discovery.Body.String())
	}
	w := request("/Items/" + themes.Items[0].ID + "/PlaybackInfo")
	if w.Code != http.StatusOK {
		t.Fatalf("discovered theme PlaybackInfo: status=%d body=%s", w.Code, w.Body.String())
	}
	var playbackInfo playbackInfoResponseDTO
	if err := json.Unmarshal(w.Body.Bytes(), &playbackInfo); err != nil {
		t.Fatal(err)
	}
	if len(playbackInfo.MediaSources) != 1 || playbackInfo.MediaSources[0].ID != themes.Items[0].ID || playbackInfo.MediaSources[0].DirectStreamURL == "" {
		t.Fatalf("discovered theme has no matching playable source: %+v", playbackInfo)
	}
}

func TestThemePlaybackInfoIDDispatch(t *testing.T) {
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		for _, native := range []string{"review-opaque-item-113", "movie-tmdb-42", "42"} {
			t.Run(method+"/"+native, func(t *testing.T) {
				codec := NewResourceIDCodec()
				id := codec.EncodeStringID(EncodedIDItem, native)
				if native == "review-opaque-item-113" && id != "0b185e50-7161-5064-b94c-451fa5a86ad7" {
					t.Fatal("fixture no longer exercises the hashed theme-prefix collision")
				}
				content := &countingContentService{}
				h := &PlaybackHandler{codec: codec, content: content, deviceProfiles: NewDeviceProfileStore(time.Hour, nil), themeSongs: &compatThemeFixture{err: themesongs.ErrNotFound}}
				router := chi.NewRouter()
				router.MethodFunc(method, "/Items/{id}/PlaybackInfo", h.HandlePlaybackInfo)
				req := httptest.NewRequest(method, "/Items/"+id+"/PlaybackInfo", nil)
				req = req.WithContext(context.WithValue(req.Context(), compatSessionKey, &Session{Token: "test-token"}))
				w := httptest.NewRecorder()
				router.ServeHTTP(w, req)
				if content.getItemDetailCalls != 1 {
					t.Fatalf("ordinary ID %q bypassed content lookup: status=%d body=%s", id, w.Code, w.Body.String())
				}
			})
		}
		canonical := EncodeNumericID(EncodedIDThemeSong, 7).String()
		for _, id := range []string{canonical, strings.ReplaceAll(canonical, "-", ""), strings.ToUpper(canonical), "urn:uuid:" + canonical, "{" + canonical + "}"} {
			t.Run(method+"/"+id, func(t *testing.T) {
				h := &PlaybackHandler{codec: NewResourceIDCodec(), themeSongs: &compatThemeFixture{file: themesongs.File{Song: themesongs.Song{ID: "7", Container: "mp3"}}}, deviceProfiles: NewDeviceProfileStore(time.Hour, nil)}
				router := chi.NewRouter()
				router.MethodFunc(method, "/Items/{id}/PlaybackInfo", h.HandlePlaybackInfo)
				req := httptest.NewRequest(method, "/Items/"+id+"/PlaybackInfo", nil)
				req = req.WithContext(context.WithValue(req.Context(), compatSessionKey, &Session{Token: "test-token"}))
				w := httptest.NewRecorder()
				router.ServeHTTP(w, req)
				if w.Code != http.StatusOK {
					t.Fatalf("theme ID rejected: status=%d body=%s", w.Code, w.Body.String())
				}
			})
		}
	}
}
