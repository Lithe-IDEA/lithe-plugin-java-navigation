package backend

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const testTimeout = 10 * time.Second

type mockHost struct {
	t      *testing.T
	in     *io.PipeWriter
	frames chan map[string]any
	done   chan int

	mu   sync.Mutex
	msgs []map[string]any
}

func startBackendSession(t *testing.T, handler func(method string, params map[string]any) (any, *rpcErr)) *mockHost {
	t.Helper()
	b, err := New("0.2.0-test")
	if err != nil {
		t.Fatalf("backend.New: %v", err)
	}
	backendInR, hostToBackendW := io.Pipe()
	hostFromBackendR, backendOutW := io.Pipe()

	m := &mockHost{
		t:      t,
		in:     hostToBackendW,
		frames: make(chan map[string]any, 64),
		done:   make(chan int, 1),
	}
	go func() {
		code := b.Serve(backendInR, backendOutW)
		_ = backendOutW.Close()
		m.done <- code
	}()
	go func() {
		r := bufio.NewReader(hostFromBackendR)
		for {
			msg, err := readFrame(r)
			if err != nil {
				close(m.frames)
				return
			}
			m.frames <- msg
		}
	}()
	t.Cleanup(func() {
		_ = hostToBackendW.Close()
		select {
		case <-m.done:
		case <-time.After(testTimeout):
			t.Error("backend did not exit")
		}
	})
	_ = handler
	return m
}

func readFrame(r *bufio.Reader) (map[string]any, error) {
	length := 0
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if v, ok := strings.CutPrefix(line, "Content-Length: "); ok {
			length, _ = strconv.Atoi(v)
		}
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(buf, &out); err != nil {
		return nil, err
	}
	return out, nil
}

type rpcErr struct {
	code int
	msg  string
}

func (m *mockHost) writeJSON(v any) {
	m.t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		m.t.Fatal(err)
	}
	if _, err := fmt.Fprintf(m.in, "Content-Length: %d\r\n\r\n%s", len(b), b); err != nil {
		m.t.Fatal(err)
	}
}

func (m *mockHost) notify(method string, params any) {
	m.writeJSON(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (m *mockHost) request(id any, method string, params any) {
	m.writeJSON(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
}

func (m *mockHost) runUntil(
	stop func(frame map[string]any) bool,
	onRequest func(method string, params map[string]any) any,
) (map[string]any, []map[string]any) {
	m.t.Helper()
	var logs []map[string]any
	deadline := time.After(testTimeout)
	for {
		select {
		case frame, ok := <-m.frames:
			if !ok {
				m.t.Fatal("backend closed protocol stream prematurely")
			}
			method, _ := frame["method"].(string)
			if method == "lithe/log" {
				if p, ok := frame["params"].(map[string]any); ok {
					logs = append(logs, p)
				}
				continue
			}
			if method != "" {
				answer := onRequest(method, mapField(frame, "params"))
				done := stop(frame)
				m.writeJSON(map[string]any{"jsonrpc": "2.0", "id": frame["id"], "result": answer})
				if done {
					return frame, logs
				}
				continue
			}
			if stop(frame) {
				return frame, logs
			}
		case <-deadline:
			m.t.Fatal("timed out waiting for backend frame")
		}
	}
}

func mapField(m map[string]any, k string) map[string]any {
	v, _ := m[k].(map[string]any)
	return v
}

func TestBackendRegistrationCollectAndInvokeWithQuietLogging(t *testing.T) {
	m := startBackendSession(t, nil)

	ctxRef := map[string]any{"contextID": "ctx-lite", "generation": 1}
	fullCtx := map[string]any{
		"contextID":  "ctx-lite",
		"generation": 1,
		"folders":    []any{map[string]any{"rootID": "r1", "path": "/workspace/lite"}},
	}

	m.request(0, "initialize", map[string]any{
		"rootUri": "file:///tmp/state/",
		"capabilities": map[string]any{
			"experimental": map[string]any{
				"litheFunctions": map[string]any{"versions": []int{0}},
				"litheLogging":   map[string]any{"versions": []int{0}},
			},
		},
	})
	initResp, _ := m.runUntil(func(f map[string]any) bool { return f["id"] == float64(0) }, func(string, map[string]any) any { return nil })
	if initResp["error"] != nil {
		t.Fatalf("initialize failed: %+v", initResp)
	}
	m.notify("initialized", map[string]any{})
	// Start through the same backend function the host uses. Java is only
	// an optional navigation input, never the language server executable.
	for i, tc := range []struct {
		tools any
		want  []string
	}{
		{nil, []string{"--state-dir", "/state"}},
		{map[string]any{"java": map[string]string{"home": "/selected/jdk 8", "version": "1.8"}}, []string{"--state-dir", "/state", "--jdk-home", "/selected/jdk 8"}},
	} {
		id := 30 + i
		m.request(id, "lithe/function/invoke", map[string]any{
			"functionID": "javaNavigation.resolveLaunch", "operationID": fmt.Sprintf("op-launch-%d", i), "context": ctxRef,
			"params": map[string]any{"serverID": "java", "stateDirectory": "/state", "platform": map[string]string{"os": "windows"}, "tools": tc.tools},
		})
		frame, _ := m.runUntil(func(f map[string]any) bool { return f["id"] == float64(id) }, func(string, map[string]any) any { return map[string]any{} })
		launch := mapField(frame, "result")
		if mapField(launch, "executable")["path"] != "bin/java-navigation.exe" || fmt.Sprint(launch["arguments"]) != fmt.Sprint(tc.want) {
			t.Fatalf("unexpected launch answer: %+v", frame)
		}
	}
	m.notify(methodDidChangeState, map[string]any{
		"serverID": "java",
		"state":    "running",
		"context":  ctxRef,
	})

	// Wait for editor/registerActions subscription.
	var regParams map[string]any
	m.runUntil(
		func(f map[string]any) bool { return f["method"] == "lithe/host/editor/registerActions" },
		func(method string, params map[string]any) any {
			switch method {
			case "lithe/host/workspace/listContexts":
				return map[string]any{"contexts": []any{fullCtx}}
			case "lithe/host/editor/registerActions":
				regParams = params
				return map[string]any{"subscriptionID": "sub-1"}
			default:
				return map[string]any{}
			}
		},
	)
	if regParams["id"] != ActionsBindingID || regParams["function"] != CollectFunctionID {
		t.Fatalf("unexpected registerActions params: %+v", regParams)
	}

	// Invoke javaNavigation.collect; verify 4 semantic icons + suppressed INFO log.
	m.request(10, "lithe/function/invoke", map[string]any{
		"functionID":  CollectFunctionID,
		"operationID": "op-collect-1",
		"context":     ctxRef,
		"params": map[string]any{
			"document": map[string]any{"path": "src/App.java"},
			"text":     "class Impl implements Greeter { public void greet() {} }\n",
		},
	})
	collectResp, collectLogs := m.runUntil(
		func(f map[string]any) bool { return f["id"] == float64(10) },
		func(method string, params map[string]any) any {
			switch method {
			case "lithe/host/workspace/getContext":
				return fullCtx
			case "lithe/host/getCapabilities":
				return map[string]any{
					"services": map[string]any{
						"editor": map[string]any{
							"options": map[string]any{
								"actionIcons": []string{
									"navigate-implemented",
									"navigate-implementing",
									"navigate-overridden",
									"navigate-overriding",
								},
							},
						},
					},
				}
			case "lithe/host/languageServer/request":
				return map[string]any{
					"value": map[string]any{
						"markers": []any{
							map[string]any{
								"line": 0, "character": 6, "endCharacter": 10,
								"direction": "up", "relation": "interface",
								"targets": []any{map[string]any{"name": "Greeter"}},
							},
							map[string]any{
								"line": 0, "character": 44, "endCharacter": 49,
								"direction": "down", "relation": "inheritance",
								"targets": []any{map[string]any{"name": "SubImpl"}},
							},
						},
					},
				}
			default:
				return map[string]any{}
			}
		},
	)
	result := mapField(collectResp, "result")
	actions, _ := result["actions"].([]any)
	if len(actions) != 2 {
		t.Fatalf("collect actions = %+v, want 2", collectResp)
	}
	first := actions[0].(map[string]any)
	second := actions[1].(map[string]any)
	if first["icon"] != "navigate-implementing" || second["icon"] != "navigate-overridden" {
		t.Fatalf("unexpected icons: %+v %+v", first["icon"], second["icon"])
	}
	for _, lg := range collectLogs {
		if lg["kind"] == "operationFinished" && lg["operation"] == CollectFunctionID {
			t.Fatalf("collect must suppress operationFinished INFO log: %+v", collectLogs)
		}
	}

	// Invoke javaNavigation.invoke; verify reveal.locations and INFO log.
	invokeSpec := first["invoke"].(map[string]any)
	m.request(11, "lithe/function/invoke", map[string]any{
		"functionID":  InvokeFunctionID,
		"operationID": "op-invoke-1",
		"context":     ctxRef,
		"params": map[string]any{
			"data": invokeSpec["params"],
		},
	})
	var requestedMethod string
	invokeResp, invokeLogs := m.runUntil(
		func(f map[string]any) bool { return f["id"] == float64(11) },
		func(method string, params map[string]any) any {
			if method == "lithe/host/languageServer/request" {
				requestedMethod, _ = params["method"].(string)
				return map[string]any{
					"value": []any{
						map[string]any{
							"uri": "file:///workspace/lite/src/Greeter.java",
							"range": map[string]any{
								"start": map[string]any{"line": 0, "character": 17},
								"end":   map[string]any{"line": 0, "character": 24},
							},
						},
					},
				}
			}
			return map[string]any{}
		},
	)
	if requestedMethod != methodLSPSuperDeclaration {
		t.Fatalf("requested LSP method = %q, want %q", requestedMethod, methodLSPSuperDeclaration)
	}
	reveal := mapField(mapField(invokeResp, "result"), "reveal")
	locs, _ := reveal["locations"].([]any)
	if len(locs) != 1 {
		t.Fatalf("reveal locations = %+v", invokeResp)
	}
	sawFinished := false
	for _, lg := range invokeLogs {
		if lg["kind"] == "operationFinished" && lg["operation"] == InvokeFunctionID {
			sawFinished = true
		}
	}
	if !sawFinished {
		t.Fatalf("expected operationFinished INFO log for invoke: %+v", invokeLogs)
	}

	m.request(99, "shutdown", nil)
	m.runUntil(func(f map[string]any) bool { return f["id"] == float64(99) }, func(string, map[string]any) any {
		return map[string]any{}
	})
	m.notify("exit", nil)
	select {
	case code := <-m.done:
		if code != 0 {
			t.Fatalf("backend exit code = %d, want 0", code)
		}
		m.done <- 0 // for t.Cleanup
	case <-time.After(testTimeout):
		t.Fatal("backend did not exit cleanly after exit")
	}
}

func TestDocumentFileURIAndLegacyIconFallback(t *testing.T) {
	cases := []struct {
		root, rel, want string
	}{
		{"/workspace/app", "src/Main.java", "file:///workspace/app/src/Main.java"},
		{"/workspace/my app", "src/Main.java", "file:///workspace/my%20app/src/Main.java"},
		{`C:\Users\dev\app`, `src\Main.java`, "file:///C:/Users/dev/app/src/Main.java"},
		{`\\server\share\repo`, `src/Main.java`, "file://server/share/repo/src/Main.java"},
	}
	for _, tc := range cases {
		got, err := documentFileURI(tc.root, tc.rel)
		if err != nil || got != tc.want {
			t.Errorf("documentFileURI(%q, %q) = (%q, %v), want %q", tc.root, tc.rel, got, err, tc.want)
		}
	}
	if got := selectIcon(false, "down", "interface"); got != "navigate-implementations" {
		t.Errorf("legacy down icon = %q", got)
	}
	if got := selectIcon(false, "up", "inheritance"); got != "navigate-super" {
		t.Errorf("legacy up icon = %q", got)
	}
}
