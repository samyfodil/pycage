package pycage

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestServer(t *testing.T, config ServerConfig) (*httptest.Server, *Engine) {
	t.Helper()
	ctx := context.Background()
	engineConfig := DefaultConfig()
	engineConfig.RuntimeMode = RuntimeModeInterpreter
	engine, err := NewEngine(ctx, engineConfig)
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(engine, config)
	httpServer := httptest.NewServer(server)
	t.Cleanup(func() {
		httpServer.Close()
		server.Close(ctx)
		engine.Close(ctx)
	})
	return httpServer, engine
}

// frames decodes an NDJSON /execute response into one map per line, which is
// exactly how the E2B clients consume it.
func frames(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var decoded []map[string]any
	scanner := bufio.NewScanner(bytes.NewReader(body))
	scanner.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var frame map[string]any
		if err := json.Unmarshal(line, &frame); err != nil {
			t.Fatalf("decode frame %q: %v", line, err)
		}
		decoded = append(decoded, frame)
	}
	return decoded
}

func execute(t *testing.T, server *httptest.Server, body string) []map[string]any {
	t.Helper()
	response, err := http.Post(server.URL+"/execute", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		payload := new(bytes.Buffer)
		payload.ReadFrom(response.Body)
		t.Fatalf("execute returned %s: %s", response.Status, payload)
	}
	payload := new(bytes.Buffer)
	payload.ReadFrom(response.Body)
	return frames(t, payload.Bytes())
}

func frameOfType(decoded []map[string]any, kind string) map[string]any {
	for _, frame := range decoded {
		if frame["type"] == kind {
			return frame
		}
	}
	return nil
}

func TestServerExecuteEmitsE2BFrames(t *testing.T) {
	server, _ := newTestServer(t, ServerConfig{})

	decoded := execute(t, server, `{"code":"print(\"hello\")\n6*7"}`)

	stdout := frameOfType(decoded, "stdout")
	if stdout == nil || stdout["text"] != "hello\n" {
		t.Errorf("stdout frame = %v, want text %q", stdout, "hello\n")
	}
	if _, ok := stdout["timestamp"]; !ok {
		t.Error("stdout frame has no timestamp; E2B's parser reads data[\"timestamp\"]")
	}
	result := frameOfType(decoded, "result")
	if result == nil || result["text"] != "42" || result["is_main_result"] != true {
		t.Errorf("result frame = %v, want text 42 and is_main_result", result)
	}
	count := frameOfType(decoded, "number_of_executions")
	if count == nil || count["execution_count"] != float64(1) {
		t.Errorf("execution count frame = %v, want 1", count)
	}
}

func TestServerExecuteReportsPythonErrorAsFrame(t *testing.T) {
	server, _ := newTestServer(t, ServerConfig{})

	decoded := execute(t, server, `{"code":"1/0"}`)
	failure := frameOfType(decoded, "error")
	if failure == nil {
		t.Fatalf("no error frame in %v", decoded)
	}
	if failure["name"] != "ZeroDivisionError" || failure["value"] != "division by zero" {
		t.Errorf("error frame = %v, want ZeroDivisionError", failure)
	}
	if traceback, _ := failure["traceback"].(string); !strings.Contains(traceback, "ZeroDivisionError") {
		t.Errorf("traceback = %q, want it to mention the exception", traceback)
	}

	// A Python exception must not retire the context.
	decoded = execute(t, server, `{"code":"'still alive'"}`)
	if result := frameOfType(decoded, "result"); result == nil || result["text"] != "'still alive'" {
		t.Errorf("context did not survive a Python exception: %v", decoded)
	}
}

func TestServerContextsAreIsolatedAndStateful(t *testing.T) {
	server, _ := newTestServer(t, ServerConfig{})

	create := func() string {
		t.Helper()
		response, err := http.Post(server.URL+"/contexts", "application/json", strings.NewReader(`{"language":"python"}`))
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusCreated {
			t.Fatalf("create context returned %s", response.Status)
		}
		var created struct {
			ID       string `json:"id"`
			Language string `json:"language"`
		}
		if err := json.NewDecoder(response.Body).Decode(&created); err != nil {
			t.Fatal(err)
		}
		if created.Language != "python" {
			t.Errorf("language = %q, want python", created.Language)
		}
		return created.ID
	}

	first, second := create(), create()
	if first == second {
		t.Fatal("two contexts share an id")
	}

	execute(t, server, `{"code":"marker = 'first only'","context_id":"`+first+`"}`)

	decoded := execute(t, server, `{"code":"marker","context_id":"`+first+`"}`)
	if result := frameOfType(decoded, "result"); result == nil || result["text"] != "'first only'" {
		t.Errorf("context is not stateful: %v", decoded)
	}

	decoded = execute(t, server, `{"code":"globals().get('marker','absent')","context_id":"`+second+`"}`)
	if result := frameOfType(decoded, "result"); result == nil || result["text"] != "'absent'" {
		t.Errorf("state leaked between contexts: %v", decoded)
	}

	request, _ := http.NewRequest(http.MethodDelete, server.URL+"/contexts/"+first, nil)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Errorf("delete context returned %s, want 204", response.Status)
	}

	response, err = http.Post(server.URL+"/execute", "application/json",
		strings.NewReader(`{"code":"1","context_id":"`+first+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Errorf("deleted context returned %s, want 404", response.Status)
	}
}

func TestServerRejectsNonPythonAndBadToken(t *testing.T) {
	server, _ := newTestServer(t, ServerConfig{AccessToken: "sesame"})

	response, err := http.Post(server.URL+"/execute", "application/json", strings.NewReader(`{"code":"1"}`))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Errorf("missing token returned %s, want 401", response.Status)
	}

	request, _ := http.NewRequest(http.MethodPost, server.URL+"/execute", strings.NewReader(`{"code":"1","language":"javascript"}`))
	request.Header.Set("X-Access-Token", "sesame")
	request.Header.Set("Content-Type", "application/json")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Errorf("javascript returned %s, want 400", response.Status)
	}

	// /health stays reachable without a token so probes keep working.
	response, err = http.Get(server.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Errorf("health returned %s, want 200", response.Status)
	}
}

func TestServerEnforcesContextLimit(t *testing.T) {
	server, _ := newTestServer(t, ServerConfig{MaxContexts: 1})

	response, err := http.Post(server.URL+"/contexts", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("first context returned %s", response.Status)
	}

	response, err = http.Post(server.URL+"/contexts", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("second context returned %s, want 503", response.Status)
	}
}
