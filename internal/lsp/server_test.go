package lsp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Lithe-IDEA/lithe-plugin-java-navigation/internal/workspace"
)

// replyTimeout bounds every wait for the server in these tests.
const replyTimeout = 10 * time.Second

// client drives a Server over in-memory pipes.
type client struct {
	t      *testing.T
	in     *io.PipeWriter
	out    *conn
	nextID int
	msgs   chan json.RawMessage
	done   chan error
}

func startServer(t *testing.T, root string) *client {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	s := New(inR, outW, Options{
		Name: "java-navigation", Version: "test", State: filepath.Join(t.TempDir(), "state"),
		Workspace: workspace.Options{Freshness: -1},
		Logf:      t.Logf,
	})
	c := &client{t: t, in: inW, out: newConn(outR, io.Discard), msgs: make(chan json.RawMessage, 64), done: make(chan error, 1)}
	go func() {
		err := s.Run()
		outW.Close()
		c.done <- err
	}()
	go func() {
		r := bufio.NewReader(outR)
		c.out.r = r
		for {
			m, err := c.readRaw()
			if err != nil {
				close(c.msgs)
				return
			}
			c.msgs <- m
		}
	}()
	t.Cleanup(func() {
		inW.Close()
		select {
		case <-c.done:
		case <-time.After(replyTimeout):
			t.Error("server did not stop")
		}
	})
	return c
}

func (c *client) readRaw() (json.RawMessage, error) {
	var n int
	for {
		line, err := c.out.r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if v, ok := strings.CutPrefix(line, "Content-Length: "); ok {
			fmt.Sscan(v, &n)
		}
	}
	b := make([]byte, n)
	_, err := io.ReadFull(c.out.r, b)
	return b, err
}

func (c *client) send(v any) {
	c.t.Helper()
	b, _ := json.Marshal(v)
	if _, err := fmt.Fprintf(c.in, "Content-Length: %d\r\n\r\n%s", len(b), b); err != nil {
		c.t.Fatal(err)
	}
}

func (c *client) notify(method string, params any) {
	c.send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

// call sends a request and returns its result, failing on an error response.
func (c *client) call(method string, params any, result any) {
	c.t.Helper()
	c.nextID++
	id := c.nextID
	c.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	deadline := time.After(replyTimeout)
	for {
		select {
		case raw, ok := <-c.msgs:
			if !ok {
				c.t.Fatalf("%s: server closed the stream", method)
			}
			var r struct {
				ID     int             `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  *responseError  `json:"error"`
			}
			if json.Unmarshal(raw, &r) != nil || r.ID != id {
				continue
			}
			if r.Error != nil {
				c.t.Fatalf("%s: error %d %s", method, r.Error.Code, r.Error.Message)
			}
			if result != nil {
				if err := json.Unmarshal(r.Result, result); err != nil {
					c.t.Fatalf("%s: %v in %s", method, err, r.Result)
				}
			}
			return
		case <-deadline:
			c.t.Fatalf("%s: no reply within %v", method, replyTimeout)
		}
	}
}

func TestServerSession(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"src/a/Shape.java": "package a;\n\npublic interface Shape {\n    double area();\n}\n",
		"src/a/Circle.java": `package a;

public class Circle implements Shape {
    private final double r;

    public Circle(double r) { this.r = r; }

    @Override
    public double area() { return Math.PI * r * r; }

    enum Unit { MM, CM }
}
`,
		"src/b/Main.java": "package b;\n\nimport a.Circle;\n\nclass Main {\n    double f() { return new Circle(1).area(); }\n}\n",
	}
	for rel, body := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(body), 0o644)
	}
	c := startServer(t, root)

	var init struct {
		Capabilities struct {
			PositionEncoding   string `json:"positionEncoding"`
			DefinitionProvider bool   `json:"definitionProvider"`
			ReferencesProvider bool   `json:"referencesProvider"`
		} `json:"capabilities"`
	}
	c.call("initialize", map[string]any{
		"processId": nil, "rootUri": uriOf(root),
		"capabilities": map[string]any{"textDocument": map[string]any{"documentSymbol": map[string]any{"hierarchicalDocumentSymbolSupport": true}}},
	}, &init)
	if init.Capabilities.PositionEncoding != "utf-16" || !init.Capabilities.DefinitionProvider || !init.Capabilities.ReferencesProvider {
		t.Fatalf("capabilities = %+v", init.Capabilities)
	}
	c.notify("initialized", map[string]any{})

	mainPath := filepath.Join(root, "src/b/Main.java")
	mainURI := uriOf(mainPath)
	c.notify("textDocument/didOpen", map[string]any{"textDocument": map[string]any{
		"uri": mainURI, "languageId": "java", "version": 1, "text": files["src/b/Main.java"]}})
	// An unsaved edit: "area()" moves right by "   " on line 5.
	c.notify("textDocument/didChange", map[string]any{
		"textDocument":   map[string]any{"uri": mainURI, "version": 2},
		"contentChanges": []any{map[string]any{"range": Range{Start: Position{5, 24}, End: Position{5, 24}}, "text": "   "}},
	})

	// "area" in "new Circle(1).area()" goes to Circle.area.
	var loc *Location
	c.call("textDocument/definition", map[string]any{
		"textDocument": map[string]any{"uri": mainURI}, "position": Position{Line: 5, Character: 41},
	}, &loc)
	if loc == nil || loc.URI != uriOf(filepath.Join(root, "src/a/Circle.java")) || loc.Range != (Range{Position{8, 18}, Position{8, 22}}) {
		t.Fatalf("definition = %+v", loc)
	}

	// References of Shape.area: Circle.area overrides it but is a separate
	// declaration, so only the interface's own declaration is listed.
	var refs []Location
	shapeURI := uriOf(filepath.Join(root, "src/a/Shape.java"))
	c.call("textDocument/references", map[string]any{
		"textDocument": map[string]any{"uri": shapeURI}, "position": Position{3, 12},
		"context": map[string]any{"includeDeclaration": true},
	}, &refs)
	if len(refs) != 1 || refs[0].URI != shapeURI {
		t.Errorf("references of Shape.area = %+v", refs)
	}
	// References of the Circle class include the import and constructor call.
	c.call("textDocument/references", map[string]any{
		"textDocument": map[string]any{"uri": uriOf(filepath.Join(root, "src/a/Circle.java"))}, "position": Position{2, 14},
		"context": map[string]any{"includeDeclaration": false},
	}, &refs)
	if len(refs) != 1 || refs[0].URI != mainURI || refs[0].Range.Start.Line != 5 {
		t.Errorf("references of Circle = %+v", refs)
	}

	// The outline of Circle, as a tree.
	var syms []struct {
		Name     string `json:"name"`
		Kind     int    `json:"kind"`
		Children []struct {
			Name string `json:"name"`
			Kind int    `json:"kind"`
		} `json:"children"`
	}
	c.call("textDocument/documentSymbol", map[string]any{"textDocument": map[string]any{"uri": uriOf(filepath.Join(root, "src/a/Circle.java"))}}, &syms)
	var outline []string
	for _, s := range syms {
		outline = append(outline, fmt.Sprintf("%s/%d", s.Name, s.Kind))
		for _, ch := range s.Children {
			outline = append(outline, fmt.Sprintf("  %s/%d", ch.Name, ch.Kind))
		}
	}
	if got, want := strings.Join(outline, ","), "Circle/5,  r/8,  Circle/9,  area/6,  Unit/10"; got != want {
		t.Errorf("outline = %s, want %s", got, want)
	}

	// An unsupported request is refused, not ignored.
	c.nextID++
	c.send(map[string]any{"jsonrpc": "2.0", "id": c.nextID, "method": "textDocument/hover", "params": map[string]any{}})

	c.call("shutdown", nil, nil)
	c.notify("exit", nil)
	select {
	case err := <-c.done:
		if err != nil {
			t.Errorf("Run = %v", err)
		}
		c.done <- nil // for Cleanup
	case <-time.After(replyTimeout):
		t.Fatal("server did not exit")
	}
}
