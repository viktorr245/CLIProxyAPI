package registry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// Contract from router-for-me/models PR #80 at 61347cf713103da87d0097ba998ce0078c32532b.
func haiku55CatalogDefinition(t *testing.T) *ModelInfo {
	t.Helper()
	var model ModelInfo
	if err := json.Unmarshal([]byte(`{
		"id":"claude-haiku-5-5","object":"model","created":1791331200,
		"owned_by":"anthropic","type":"claude","display_name":"Claude Haiku 5.5",
		"description":"Fastest model for high-volume, latency-sensitive tasks such as classification, extraction, and routing",
		"context_length":1000000,"max_completion_tokens":128000,
		"thinking":{"zero_allowed":true,"dynamic_allowed":true,"levels":["low","medium","high","xhigh","max"]},
		"supportedInputModalities":["text","image"],"supportedOutputModalities":["text"]
	}`), &model); err != nil {
		t.Fatal(err)
	}
	return &model
}

// These tests are deliberately serial because the catalogue is process-global.
func setHaiku55TestCatalog(t *testing.T, data *staticModelsJSON) {
	t.Helper()
	modelsCatalogStore.mu.Lock()
	original := modelsCatalogStore.data
	modelsCatalogStore.data = data
	modelsCatalogStore.mu.Unlock()
	t.Cleanup(func() {
		modelsCatalogStore.mu.Lock()
		modelsCatalogStore.data = original
		modelsCatalogStore.mu.Unlock()
	})
}

func requireHaiku55Model(t *testing.T, models []*ModelInfo, want *ModelInfo) *ModelInfo {
	t.Helper()
	var found *ModelInfo
	count := 0
	for _, model := range models {
		if model != nil && model.ID == want.ID {
			found = model
			count++
		}
	}
	if count != 1 {
		t.Fatalf("%s: got %d definitions, want exactly one", want.ID, count)
	}
	if !reflect.DeepEqual(found, want) {
		t.Fatalf("definition = %+v, want %+v", found, want)
	}
	return found
}

func TestClaudeHaiku55Fallback(t *testing.T) {
	var embedded staticModelsJSON
	if err := json.Unmarshal(embeddedModelsJSON, &embedded); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		data *staticModelsJSON
	}{
		{name: "missing Claude section", data: &staticModelsJSON{}},
		{name: "current embedded catalogue", data: &embedded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setHaiku55TestCatalog(t, tc.data)
			original := cloneModelInfos(tc.data.Claude)
			models := GetClaudeModels()
			requireHaiku55Model(t, models, haiku55CatalogDefinition(t))
			if len(models) != len(original)+1 {
				t.Fatal("fallback changed the number of existing Claude definitions")
			}
			if len(original) > 0 && !reflect.DeepEqual(models[:len(original)], original) {
				t.Fatal("fallback changed existing Claude definitions or order")
			}
			if !reflect.DeepEqual(tc.data.Claude, original) {
				t.Fatal("fallback mutated the stored catalogue")
			}
			if got := GetGeminiModels(); !reflect.DeepEqual(got, tc.data.Gemini) {
				t.Fatal("fallback changed another provider's catalogue")
			}
			if got := LookupStaticModelInfoByChannel("claude-haiku-5-5", "gemini"); got != nil {
				t.Fatal("Claude fallback leaked into another provider's static lookup")
			}
			r := newTestModelRegistry()
			r.RegisterClient("haiku55-fallback", "claude", models)
			if got := r.GetModelProviders("claude-haiku-5-5"); !reflect.DeepEqual(got, []string{"claude"}) {
				t.Fatalf("registered providers = %v, want [claude]", got)
			}
			requireHaiku55Model(t, []*ModelInfo{r.GetModelInfo("claude-haiku-5-5", "claude")}, haiku55CatalogDefinition(t))
		})
	}
}

func TestClaudeHaiku55LookupPrecedenceAndIsolation(t *testing.T) {
	for _, remote := range []bool{false, true} {
		name := "fallback"
		if remote {
			name = "remote definition wins"
		}
		t.Run(name, func(t *testing.T) {
			want := haiku55CatalogDefinition(t)
			data := &staticModelsJSON{}
			if remote {
				want.DisplayName = "Remote Haiku revision"
				want.ContextLength = 2000000
				want.MaxCompletionTokens = 64000
				want.Thinking = &ThinkingSupport{Levels: []string{"medium", "high"}}
				want.SupportedInputModalities = []string{"text"}
				want.Config = &ModelConfig{OverrideHeader: map[string]string{"x-test-catalog": "remote"}}
				data.Claude = []*ModelInfo{cloneModelInfo(want)}
			}
			setHaiku55TestCatalog(t, data)
			original := cloneModelInfos(data.Claude)
			for _, lookup := range []struct {
				name string
				get  func() []*ModelInfo
			}{
				{name: "registration catalogue", get: GetClaudeModels},
				{name: "channel catalogue", get: func() []*ModelInfo { return GetStaticModelDefinitionsByChannel("claude") }},
				{name: "static lookup", get: func() []*ModelInfo { return []*ModelInfo{LookupStaticModelInfo(want.ID)} }},
				{name: "channel lookup", get: func() []*ModelInfo { return []*ModelInfo{LookupStaticModelInfoByChannel(want.ID, "claude")} }},
				{name: "thinking lookup", get: func() []*ModelInfo { return []*ModelInfo{LookupModelInfo(want.ID, "claude")} }},
			} {
				t.Run(lookup.name, func(t *testing.T) {
					model := requireHaiku55Model(t, lookup.get(), want)
					model.ID = "mutated"
					model.Thinking.ZeroAllowed = !model.Thinking.ZeroAllowed
					model.Thinking.Levels[0] = "mutated"
					model.SupportedInputModalities[0] = "mutated"
					model.SupportedOutputModalities[0] = "mutated"
					if model.Config != nil {
						model.Config.OverrideHeader["x-test-catalog"] = "mutated"
					}
					requireHaiku55Model(t, lookup.get(), want)
					requireHaiku55Model(t, GetClaudeModels(), want)
					if !reflect.DeepEqual(data.Claude, original) {
						t.Fatal("caller mutation leaked into the stored catalogue")
					}
				})
			}
		})
	}
}

func TestClaudeHaiku55RemoteUpdater(t *testing.T) {
	initial := &staticModelsJSON{
		Claude: []*ModelInfo{{ID: "claude-existing", DisplayName: "Original"}},
		Gemini: []*ModelInfo{{ID: "gemini-existing", DisplayName: "Original"}},
	}
	setHaiku55TestCatalog(t, initial)
	originalURLs := modelsURLs
	refreshCallbackMu.Lock()
	originalCallback, originalPending := refreshCallback, pendingRefreshChanges
	refreshCallback, pendingRefreshChanges = nil, nil
	refreshCallbackMu.Unlock()
	t.Cleanup(func() {
		modelsURLs = originalURLs
		refreshCallbackMu.Lock()
		refreshCallback, pendingRefreshChanges = originalCallback, originalPending
		refreshCallbackMu.Unlock()
	})

	r := newTestModelRegistry()
	r.RegisterClient("haiku55-refresh", "claude", GetClaudeModels())
	var notifications [][]string
	SetModelRefreshCallback(func(providers []string) {
		notifications = append(notifications, append([]string(nil), providers...))
		for _, provider := range providers {
			if provider == "claude" {
				r.RegisterClient("haiku55-refresh", "claude", GetClaudeModels())
			}
		}
	})

	fallback := haiku55CatalogDefinition(t)
	remote := cloneModelInfo(fallback)
	remote.DisplayName = "Authoritative remote Haiku"
	remote.Thinking.Levels = []string{"low", "high"}
	missing := &staticModelsJSON{
		Claude: []*ModelInfo{{ID: "claude-existing", DisplayName: "Remote"}},
		Gemini: []*ModelInfo{{ID: "gemini-existing", DisplayName: "Remote"}},
	}
	present := &staticModelsJSON{Claude: append(cloneModelInfos(missing.Claude), remote), Gemini: missing.Gemini}
	invalid := &staticModelsJSON{Claude: []*ModelInfo{remote, remote}}
	for _, tc := range []struct {
		name      string
		catalog   *staticModelsJSON
		status    int
		refresh   func(context.Context)
		want      *ModelInfo
		changed   []string
		wantStore *staticModelsJSON
	}{
		{"startup missing Haiku", missing, http.StatusOK, tryStartupRefresh, fallback, []string{"claude", "gemini", "gemini-interactions"}, missing},
		{"periodic unchanged", missing, http.StatusOK, tryPeriodicRefresh, fallback, nil, missing},
		{"periodic adds remote definition", present, http.StatusOK, tryPeriodicRefresh, remote, []string{"claude"}, present},
		{"periodic removes remote definition", missing, http.StatusOK, tryPeriodicRefresh, fallback, []string{"claude"}, missing},
		{"invalid remote rejected", invalid, http.StatusOK, tryPeriodicRefresh, fallback, nil, missing},
		{"failed remote preserves catalogue", nil, http.StatusServiceUnavailable, tryPeriodicRefresh, fallback, nil, missing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.catalog)
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write(raw)
			}))
			defer server.Close()
			modelsURLs = []string{server.URL + "/models.json"}
			notifications = nil
			tc.refresh(context.Background())
			var wantNotifications [][]string
			if tc.changed != nil {
				wantNotifications = [][]string{tc.changed}
			}
			if !reflect.DeepEqual(notifications, wantNotifications) {
				t.Errorf("refresh callbacks = %v, want %v", notifications, wantNotifications)
			}
			if !reflect.DeepEqual(getModels(), tc.wantStore) {
				t.Error("remote refresh did not retain the expected raw catalogue")
			}
			requireHaiku55Model(t, GetClaudeModels(), tc.want)
			requireHaiku55Model(t, []*ModelInfo{LookupStaticModelInfo(tc.want.ID)}, tc.want)
			requireHaiku55Model(t, []*ModelInfo{r.GetModelInfo(tc.want.ID, "claude")}, tc.want)
			if got := r.GetModelInfo("claude-existing", "claude"); !reflect.DeepEqual(got, missing.Claude[0]) {
				t.Fatal("existing model did not receive remote metadata")
			}
			if got := GetGeminiModels(); !reflect.DeepEqual(got, missing.Gemini) {
				t.Fatal("other provider did not receive remote metadata")
			}
		})
	}
}
