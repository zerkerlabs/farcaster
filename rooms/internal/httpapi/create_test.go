package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandleCreateRoom(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		body       any
		tenantID   string
		wantStatus int
		checkBody  func(t *testing.T, body map[string]any)
	}{
		{
			name:       "201 with goal only uses the default turn budget",
			body:       map[string]any{"goal": "ship the thing"},
			tenantID:   tenantA,
			wantStatus: http.StatusCreated,
			checkBody: func(t *testing.T, body map[string]any) {
				t.Helper()
				if body["id"] == "" || body["id"] == nil {
					t.Error("id is empty")
				}
				if body["goal"] != "ship the thing" {
					t.Errorf("goal = %v, want %q", body["goal"], "ship the thing")
				}
				if body["state"] != "open" {
					t.Errorf("state = %v, want %q", body["state"], "open")
				}
				if body["turn_budget"] != float64(50) {
					t.Errorf("turn_budget = %v, want 50", body["turn_budget"])
				}
				if body["members"] == nil {
					t.Error("members is missing")
				}
				if body["transcript"] == nil {
					t.Error("transcript is missing")
				}
			},
		},
		{
			name:       "201 honors an explicit turn budget",
			body:       map[string]any{"goal": "ship the thing", "turn_budget": 5},
			tenantID:   tenantA,
			wantStatus: http.StatusCreated,
			checkBody: func(t *testing.T, body map[string]any) {
				t.Helper()
				if body["turn_budget"] != float64(5) {
					t.Errorf("turn_budget = %v, want 5", body["turn_budget"])
				}
			},
		},
		{
			name:       "400 missing goal",
			body:       map[string]any{},
			tenantID:   tenantA,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "400 empty goal",
			body:       map[string]any{"goal": ""},
			tenantID:   tenantA,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "400 malformed JSON",
			body:       []byte(`{"goal": `),
			tenantID:   tenantA,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "400 zero turn budget",
			body:       map[string]any{"goal": "goal", "turn_budget": 0},
			tenantID:   tenantA,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "400 negative turn budget",
			body:       map[string]any{"goal": "goal", "turn_budget": -1},
			tenantID:   tenantA,
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "401 no tenant in context",
			body:       map[string]any{"goal": "goal"},
			tenantID:   "",
			wantStatus: http.StatusUnauthorized,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			mux, _ := newMux(t)
			req := requestAs(t, http.MethodPost, "/v1/rooms", tt.body, tt.tenantID)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body = %s", rec.Code, tt.wantStatus, rec.Body.String())
			}
			if tt.checkBody != nil {
				tt.checkBody(t, decodeBody(t, rec))
			}
		})
	}
}
