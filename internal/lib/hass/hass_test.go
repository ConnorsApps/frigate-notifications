package hass

import (
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNotify(t *testing.T) {
	var (
		gotPath string
		gotBody map[string]any
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if err := json.UnmarshalRead(r.Body, &gotBody); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := New(srv.URL, "test-token")

	err := c.Notify(context.Background(), "mobile_app_alices_phone", map[string]any{
		"message": "person detected on front_porch",
	})
	if err != nil {
		t.Fatalf("Notify: %v", err)
	}

	const wantPath = "/api/services/notify/mobile_app_alices_phone"
	if gotPath != wantPath {
		t.Errorf("path = %q, want %q", gotPath, wantPath)
	}
	if gotBody["message"] != "person detected on front_porch" {
		t.Errorf("body[message] = %v, want %q", gotBody["message"], "person detected on front_porch")
	}
}
