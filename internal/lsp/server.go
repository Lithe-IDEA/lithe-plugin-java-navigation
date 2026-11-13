// Package lsp serves go to definition, find references and the document
// outline for Java over the Language Server Protocol, on top of package
// workspace. It speaks only what those features need: document
// synchronization, the three requests, workspace folders and cancellation.
package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/Lithe-IDEA/lithe-plugin-java-navigation/internal/codenav"
	"github.com/Lithe-IDEA/lithe-plugin-java-navigation/internal/jdk"
	"github.com/Lithe-IDEA/lithe-plugin-java-navigation/internal/workspace"
)

// Options configure a Server.
type Options struct {
	Name, Version string
	// State is the writable directory for extracted JDK sources.
	State string
	// FindJDK locates the JDK whose sources are used; nil disables them.
	FindJDK func(context.Context) (jdk.JDK, bool)
	// Dependencies enables navigation into dependency jars (see
	// workspace.Options.Dependencies).
	Dependencies bool
	// MavenRepo is the local Maven repository; "" leaves Maven dependencies
	// out.
	MavenRepo string
	// Workspace overrides workspace options other than Root, for tests.
	Workspace workspace.Options
	// Logf writes a diagnostic line; nil discards it.
	Logf func(format string, args ...any)
}

// Server is one LSP session.
type Server struct {
	opt  Options
	conn *conn

	mu           sync.Mutex
	initialized  bool
	shutdown     bool
	enc          encoding
	hierarchical bool // the client takes DocumentSymbol trees
	docs         map[string]*document
	folders      []*workspace.Workspace
	inflight     map[string]context.CancelFunc
	wg           sync.WaitGroup
}

// New returns a server that reads requests from r and writes to w.
func New(r io.Reader, w io.Writer, opt Options) *Server {
	if opt.Logf == nil {
		opt.Logf = func(string, ...any) {}
	}
	return &Server{opt: opt, conn: newConn(r, w), docs: map[string]*document{}, inflight: map[string]context.CancelFunc{}}
}

// ErrExitWithoutShutdown is Run's result for an exit notification that was
// not preceded by shutdown; the process should then exit with status 1.
var ErrExitWithoutShutdown = errors.New("exit without shutdown")

// Run serves until the client sends exit or closes the stream.
func (s *Server) Run() error {
	defer func() {
		// Queries still running are answered as cancelled before Run returns.
		s.mu.Lock()
		for _, cancel := range s.inflight {
			cancel()
		}
		s.mu.Unlock()
		s.wg.Wait()
	}()
	for {
		m, err := s.conn.read()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if errors.Is(err, errParse) {
			s.conn.replyError(nil, codeParseError, "invalid JSON")
			continue
		}
		if err != nil {
			return err
		}
		if m.Method == "exit" {
			s.mu.Lock()
			clean := s.shutdown
			s.mu.Unlock()
			if !clean {
				return ErrExitWithoutShutdown
			}
			return nil
		}
		if m.Method == "" {
			continue // a response; this server sends no requests
		}
		s.dispatch(m)
	}
}

// dispatch handles a message in arrival order. Notifications change the
// documents synchronously, so a later request sees their effect; a query
// takes a snapshot of its document here and is answered on its own
// goroutine.
func (s *Server) dispatch(m *message) {
	s.mu.Lock()
	ready, down := s.initialized, s.shutdown
	s.mu.Unlock()
	if m.Method == "initialize" {
		s.reply(m, s.initialize(m.Params))
		return
	}
	if !ready {
		if m.isRequest() {
			s.conn.replyError(m.ID, codeServerNotInit, "server not initialized")
		}
		return
	}
	if down && m.isRequest() {
		s.conn.replyError(m.ID, codeInvalidRequest, "server is shutting down")
		return
	}
	switch m.Method {
	case "initialized":
	case "shutdown":
		s.mu.Lock()
		s.shutdown = true
		s.mu.Unlock()
		s.conn.reply(m.ID, nil)
	case "textDocument/didOpen":
		s.didOpen(m.Params)
	case "textDocument/didChange":
		s.didChange(m.Params)
	case "textDocument/didClose":
		s.didClose(m.Params)
	case "textDocument/didSave":
	case "workspace/didChangeWatchedFiles":
		for _, w := range s.workspaces() {
			w.Invalidate()
		}
	case "workspace/didChangeWorkspaceFolders":
		s.changeFolders(m.Params)
	case "workspace/didChangeConfiguration":
	case "$/cancelRequest":
		s.cancel(m.Params)
	case "textDocument/definition":
		s.query(m, s.definition)
	case "textDocument/references":
		s.query(m, s.references)
	case "textDocument/documentSymbol":
		s.query(m, s.documentSymbol)
	default:
		if m.isRequest() {
			s.conn.replyError(m.ID, codeMethodNotFound, "method not supported: "+m.Method)
		}
	}
}

// reply answers m with result, or with the error result carries.
func (s *Server) reply(m *message, result any) {
	if err, ok := result.(*rpcError); ok {
		s.conn.replyError(m.ID, err.code, err.msg)
		return
	}
	s.conn.reply(m.ID, result)
}

type rpcError struct {
	code int
	msg  string
}

func (e *rpcError) Error() string { return e.msg }

// handler answers a query from a snapshot taken in arrival order.
type handler func(params json.RawMessage) (func(ctx context.Context) (any, error), error)

func (s *Server) query(m *message, h handler) {
	run, err := h(m.Params)
	if err != nil {
		s.conn.replyError(m.ID, codeInvalidParams, err.Error())
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	key := string(m.ID)
	s.mu.Lock()
	s.inflight[key] = cancel
	s.mu.Unlock()
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() {
			s.mu.Lock()
			delete(s.inflight, key)
			s.mu.Unlock()
			cancel()
		}()
		result, err := run(ctx)
		switch {
		case ctx.Err() != nil:
			s.conn.replyError(m.ID, codeRequestCancelled, "request cancelled")
		case err != nil:
			s.opt.Logf("%s: %v", m.Method, err)
			s.conn.replyError(m.ID, codeInternalError, err.Error())
		default:
			s.conn.reply(m.ID, result)
		}
	}()
}

func (s *Server) cancel(params json.RawMessage) {
	var p struct {
		ID json.RawMessage `json:"id"`
	}
	if json.Unmarshal(params, &p) != nil {
		return
	}
	s.mu.Lock()
	cancel := s.inflight[string(p.ID)]
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// --- initialize ---

type workspaceFolder struct {
	URI  string `json:"uri"`
	Name string `json:"name"`
}

func (s *Server) initialize(params json.RawMessage) any {
	var p struct {
		RootURI          string            `json:"rootUri"`
		RootPath         string            `json:"rootPath"`
		WorkspaceFolders []workspaceFolder `json:"workspaceFolders"`
		Capabilities     struct {
			General struct {
				PositionEncodings []string `json:"positionEncodings"`
			} `json:"general"`
			TextDocument struct {
				DocumentSymbol struct {
					Hierarchical bool `json:"hierarchicalDocumentSymbolSupport"`
				} `json:"documentSymbol"`
			} `json:"textDocument"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return &rpcError{codeInvalidParams, "initialize: " + err.Error()}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.initialized {
		return &rpcError{codeInvalidRequest, "initialize sent twice"}
	}
	s.enc = utf16Units
	for _, e := range p.Capabilities.General.PositionEncodings {
		if e == "utf-8" {
			s.enc = utf8Bytes
			break
		}
	}
	s.hierarchical = p.Capabilities.TextDocument.DocumentSymbol.Hierarchical
	var roots []string
	for _, f := range p.WorkspaceFolders {
		if path, err := pathOf(f.URI); err == nil {
			roots = append(roots, path)
		}
	}
	if len(roots) == 0 {
		if path, err := pathOf(p.RootURI); err == nil {
			roots = append(roots, path)
		} else if p.RootPath != "" {
			roots = append(roots, filepath.Clean(p.RootPath))
		}
	}
	for _, r := range roots {
		s.folders = append(s.folders, s.newWorkspace(r))
	}
	s.initialized = true
	s.opt.Logf("initialized: %d folder(s), position encoding %s", len(roots), s.enc)
	return map[string]any{
		"capabilities": map[string]any{
			"positionEncoding": s.enc.String(),
			"textDocumentSync": map[string]any{
				"openClose": true,
				"change":    2, // incremental
			},
			"definitionProvider":     true,
			"referencesProvider":     true,
			"documentSymbolProvider": true,
			"workspace": map[string]any{
				"workspaceFolders": map[string]any{"supported": true, "changeNotifications": true},
			},
		},
		"serverInfo": map[string]any{"name": s.opt.Name, "version": s.opt.Version},
	}
}

func (s *Server) newWorkspace(root string) *workspace.Workspace {
	o := s.opt.Workspace
	o.Root = root
	o.State = s.opt.State
	o.FindJDK = s.opt.FindJDK
	o.Dependencies = s.opt.Dependencies
	o.MavenRepo = s.opt.MavenRepo
	if o.Logf == nil {
		o.Logf = s.opt.Logf
	}
	return workspace.New(o)
}

func (s *Server) changeFolders(params json.RawMessage) {
	var p struct {
		Event struct {
			Added   []workspaceFolder `json:"added"`
			Removed []workspaceFolder `json:"removed"`
		} `json:"event"`
	}
	if json.Unmarshal(params, &p) != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range p.Event.Removed {
		path, err := pathOf(f.URI)
		if err != nil {
			continue
		}
		kept := s.folders[:0]
		for _, w := range s.folders {
			if w.Root() != path {
				kept = append(kept, w)
			}
		}
		s.folders = kept
	}
	for _, f := range p.Event.Added {
		if path, err := pathOf(f.URI); err == nil {
			s.folders = append(s.folders, s.newWorkspace(path))
		}
	}
}

func (s *Server) workspaces() []*workspace.Workspace {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*workspace.Workspace(nil), s.folders...)
}

// locate returns the workspace that answers for path and how it sees the
// file: the innermost folder containing it, or the one whose JDK sources it
// is a copy of.
func (s *Server) locate(path string) (*workspace.Workspace, workspace.Doc) {
	var best *workspace.Workspace
	var bestDoc workspace.Doc
	for _, w := range s.workspaces() {
		d := w.DocOf(path)
		if d.Library != "" {
			return w, d
		}
		if d.Rel != "" && (best == nil || len(w.Root()) > len(best.Root())) {
			best, bestDoc = w, d
		}
	}
	return best, bestDoc
}

// --- document synchronization ---

type textDocumentItem struct {
	URI     string `json:"uri"`
	Version int    `json:"version"`
	Text    string `json:"text"`
}

type textDocumentIdentifier struct {
	URI string `json:"uri"`
}

func (s *Server) didOpen(params json.RawMessage) {
	var p struct {
		TextDocument textDocumentItem `json:"textDocument"`
	}
	if json.Unmarshal(params, &p) != nil {
		return
	}
	path, err := pathOf(p.TextDocument.URI)
	if err != nil {
		return
	}
	d := &document{uri: p.TextDocument.URI, path: path, version: p.TextDocument.Version, text: []byte(p.TextDocument.Text)}
	s.mu.Lock()
	s.docs[d.uri] = d
	s.mu.Unlock()
	if w, doc := s.locate(path); w != nil {
		w.Open(doc, d.text)
	}
}

func (s *Server) didChange(params json.RawMessage) {
	var p struct {
		TextDocument struct {
			URI     string `json:"uri"`
			Version int    `json:"version"`
		} `json:"textDocument"`
		ContentChanges []contentChange `json:"contentChanges"`
	}
	if json.Unmarshal(params, &p) != nil {
		return
	}
	s.mu.Lock()
	d := s.docs[p.TextDocument.URI]
	enc := s.enc
	var text []byte
	if d != nil {
		if err := d.apply(p.ContentChanges, enc); err != nil {
			s.opt.Logf("didChange %s: %v", d.uri, err)
		}
		d.version = p.TextDocument.Version
		text = d.text
	}
	s.mu.Unlock()
	if d == nil {
		return
	}
	if w, doc := s.locate(d.path); w != nil {
		w.Open(doc, text)
	}
}

func (s *Server) didClose(params json.RawMessage) {
	var p struct {
		TextDocument textDocumentIdentifier `json:"textDocument"`
	}
	if json.Unmarshal(params, &p) != nil {
		return
	}
	s.mu.Lock()
	d := s.docs[p.TextDocument.URI]
	delete(s.docs, p.TextDocument.URI)
	s.mu.Unlock()
	if d == nil {
		return
	}
	if w, doc := s.locate(d.path); w != nil {
		w.Close(doc)
	}
}

// snapshot returns the content of uri: the open document's text, otherwise
// the file on disk.
func (s *Server) snapshot(uri string) (path string, text []byte, err error) {
	s.mu.Lock()
	d := s.docs[uri]
	if d != nil {
		path, text = d.path, d.text
	}
	s.mu.Unlock()
	if d != nil {
		return path, text, nil
	}
	path, err = pathOf(uri)
	if err != nil {
		return "", nil, err
	}
	text, err = os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", nil, err
	}
	return path, text, nil
}

// --- queries ---

type textDocumentPosition struct {
	TextDocument textDocumentIdentifier `json:"textDocument"`
	Position     Position               `json:"position"`
}

// Location is a range in a file.
type Location struct {
	URI   string `json:"uri"`
	Range Range  `json:"range"`
}

func (s *Server) encoding() encoding {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.enc
}

// prepare snapshots a position query; covered is false for a document
// navigation does not answer.
func (s *Server) prepare(p textDocumentPosition) (w *workspace.Workspace, doc workspace.Doc, text []byte, off int, err error) {
	path, text, err := s.snapshot(p.TextDocument.URI)
	if err != nil {
		return nil, doc, nil, 0, err
	}
	w, doc = s.locate(path)
	return w, doc, text, offsetOf(text, p.Position, s.encoding()), nil
}

func (s *Server) definition(params json.RawMessage) (func(context.Context) (any, error), error) {
	var p textDocumentPosition
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	w, doc, text, off, err := s.prepare(p)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context) (any, error) {
		if w == nil || !doc.Covered() {
			return nil, nil
		}
		t, ok, err := w.Definition(ctx, doc, text, off)
		if err != nil || !ok {
			return nil, err
		}
		loc, err := s.location(w, t)
		if err != nil {
			return nil, err
		}
		return loc, nil
	}, nil
}

func (s *Server) references(params json.RawMessage) (func(context.Context) (any, error), error) {
	var p struct {
		textDocumentPosition
		Context struct {
			IncludeDeclaration bool `json:"includeDeclaration"`
		} `json:"context"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	w, doc, text, off, err := s.prepare(p.textDocumentPosition)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context) (any, error) {
		out := []Location{}
		if w == nil || !doc.Covered() {
			return out, nil
		}
		targets, more, err := w.References(ctx, doc, text, off, p.Context.IncludeDeclaration)
		if err != nil {
			return nil, err
		}
		if more {
			s.opt.Logf("references: stopped at %d", workspace.MaxReferences)
		}
		texts := map[string][]byte{}
		for _, t := range targets {
			loc, err := s.locationCached(w, t, texts)
			if err != nil {
				continue
			}
			out = append(out, loc)
		}
		return out, nil
	}, nil
}

// location converts a target to a protocol location, reading the target's
// text to measure its columns.
func (s *Server) location(w *workspace.Workspace, t workspace.Target) (Location, error) {
	return s.locationCached(w, t, nil)
}

func (s *Server) locationCached(w *workspace.Workspace, t workspace.Target, texts map[string][]byte) (Location, error) {
	text, ok := texts[t.File]
	if !ok {
		var err error
		if text, err = s.textOf(w, t.File); err != nil {
			return Location{}, err
		}
		if texts != nil {
			texts[t.File] = text
		}
	}
	return Location{URI: uriOf(t.File), Range: nameRange(text, t.Line, t.Column, s.encoding())}, nil
}

// textOf is the content of file as the editor sees it.
func (s *Server) textOf(w *workspace.Workspace, file string) ([]byte, error) {
	s.mu.Lock()
	for _, d := range s.docs {
		if d.path == file {
			text := d.text
			s.mu.Unlock()
			return text, nil
		}
	}
	s.mu.Unlock()
	return w.Text(file)
}

// documentSymbol answers with the declarations of a Java file, as a tree when
// the client takes one.
func (s *Server) documentSymbol(params json.RawMessage) (func(context.Context) (any, error), error) {
	var p struct {
		TextDocument textDocumentIdentifier `json:"textDocument"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, err
	}
	path, text, err := s.snapshot(p.TextDocument.URI)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	hierarchical, enc := s.hierarchical, s.enc
	s.mu.Unlock()
	uri := p.TextDocument.URI
	return func(context.Context) (any, error) {
		if !codenav.Supports(path) {
			return []any{}, nil
		}
		syms := codenav.Symbols(text)
		if hierarchical {
			return documentSymbols(text, syms, enc), nil
		}
		var flat []symbolInformation
		flattenSymbols(text, uri, "", syms, enc, &flat)
		if flat == nil {
			flat = []symbolInformation{}
		}
		return flat, nil
	}, nil
}

type documentSymbolJSON struct {
	Name           string               `json:"name"`
	Detail         string               `json:"detail,omitempty"`
	Kind           int                  `json:"kind"`
	Range          Range                `json:"range"`
	SelectionRange Range                `json:"selectionRange"`
	Children       []documentSymbolJSON `json:"children,omitempty"`
}

type symbolInformation struct {
	Name          string   `json:"name"`
	Kind          int      `json:"kind"`
	Location      Location `json:"location"`
	ContainerName string   `json:"containerName,omitempty"`
}

func documentSymbols(text []byte, syms []codenav.Symbol, enc encoding) []documentSymbolJSON {
	out := make([]documentSymbolJSON, 0, len(syms))
	for _, sym := range syms {
		r := nameRange(text, sym.Line, sym.Column, enc)
		out = append(out, documentSymbolJSON{
			Name: sym.Name, Detail: sym.Detail, Kind: symbolKind(sym.Kind),
			Range: r, SelectionRange: r, Children: documentSymbols(text, sym.Children, enc),
		})
	}
	return out
}

func flattenSymbols(text []byte, uri, container string, syms []codenav.Symbol, enc encoding, out *[]symbolInformation) {
	for _, sym := range syms {
		*out = append(*out, symbolInformation{
			Name: sym.Name, Kind: symbolKind(sym.Kind), ContainerName: container,
			Location: Location{URI: uri, Range: nameRange(text, sym.Line, sym.Column, enc)},
		})
		name := sym.Name
		if container != "" {
			name = container + "." + sym.Name
		}
		flattenSymbols(text, uri, name, sym.Children, enc, out)
	}
}

// symbolKind maps to the protocol's SymbolKind numbers.
func symbolKind(k codenav.SymbolKind) int {
	switch k {
	case codenav.SymbolInterface, codenav.SymbolAnnotation:
		return 11
	case codenav.SymbolEnum:
		return 10
	case codenav.SymbolRecord:
		return 23 // Struct
	case codenav.SymbolMethod:
		return 6
	case codenav.SymbolConstructor:
		return 9
	case codenav.SymbolField:
		return 8
	case codenav.SymbolEnumConstant:
		return 22
	default:
		return 5 // Class
	}
}

// String describes the server for a log line.
func (s *Server) String() string {
	var roots []string
	for _, w := range s.workspaces() {
		roots = append(roots, w.Root())
	}
	return fmt.Sprintf("%s %s [%s]", s.opt.Name, s.opt.Version, strings.Join(roots, ", "))
}
