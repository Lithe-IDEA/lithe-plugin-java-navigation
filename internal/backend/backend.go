// Package backend implements the Lithe plugin-backend process mode
// (`--backend`) for Java Support Lite, registering bidirectional
// inheritance/implementation gutter actions via editor/registerActions.
package backend

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Lithe-IDEA/lithe-plugin-java-navigation/internal/lithesdk/lithe"
)

const (
	// BackendName is reported as serverInfo.name.
	BackendName = "java-navigation-backend"

	// ServerID matches languageServers[0].languageID in plugin.json.
	ServerID = "java"

	// ActionsBindingID identifies the editor actions subscription per context.
	ActionsBindingID = "java-navigation.markers"

	// CollectFunctionID is called by the host to collect gutter navigation actions.
	CollectFunctionID = "javaNavigation.collect"

	// InvokeFunctionID is called by the host when a user clicks a navigation gutter icon.
	InvokeFunctionID = "javaNavigation.invoke"

	// Plugin error codes returned cleanly to the host on failures.
	errInvalidParams        = "nav.invalid_params"
	errContextMissing       = "nav.context_missing"
	errWorkspaceUnavailable = "nav.workspace_unavailable"
	errLanguageServerFailed = "nav.language_server_failed"

	// Controlled degraded diagnostic categories reported through SDK Diagnostics.
	catActionsRegistrationFailed = "nav.actions_registration_failed"
	catContextsUnavailable       = "nav.contexts_unavailable"

	methodDidChangeState      = "lithe/host/languageServer/didChangeState"
	methodLanguageServerReq   = "lithe/host/languageServer/request"
	methodLSPImplementation   = "textDocument/implementation"
	methodLSPSuperDeclaration = "javaNavigation/superDeclarations"
	methodLSPMarkers          = "javaNavigation/markers"

	collectTimeoutMs   = 4000
	invokeTimeoutMs    = 5000
	syncRetryAttempts  = 20
	syncRetryInterval  = 200 * time.Millisecond
	maxActionsPerFile  = 1000
	notificationOpName = "notification"
)

type registration struct {
	generation   uint64
	subscription *lithe.Subscription
}

// Service manages editor action subscriptions across workspace contexts.
type Service struct {
	handle *lithe.BackendHandle

	mu            sync.Mutex
	registrations map[string]registration
	closed        bool
	stopCh        chan struct{}
	wg            sync.WaitGroup
}

// Run starts the plugin backend over stdin/stdout and returns its exit code.
func Run(version string) int {
	b, err := New(version)
	if err != nil {
		return 1
	}
	return b.Listen()
}

// New constructs a configured Backend ready to Serve/Listen.
func New(version string) (*lithe.Backend, error) {
	b := lithe.NewBackend(BackendName)
	if version != "" {
		b.Version(version)
	}
	svc := &Service{
		handle:        b.Handle(),
		registrations: map[string]registration{},
		stopCh:        make(chan struct{}),
	}

	if err := b.Function(CollectFunctionID, svc.collect); err != nil {
		return nil, err
	}
	if err := b.Function("javaNavigation.resolveLaunch", resolveLaunch); err != nil {
		return nil, err
	}
	if err := b.SuppressSuccessDiagnostics(CollectFunctionID); err != nil {
		return nil, err
	}
	if err := b.Function(InvokeFunctionID, svc.invoke); err != nil {
		return nil, err
	}

	b.OnInitialize(func(_ *lithe.Initialize) error {
		svc.syncAfterInitialize()
		return nil
	})
	b.OnContextsChanged(func(_ json.RawMessage) error {
		svc.syncContextsReporting()
		return nil
	})
	if err := b.OnHostNotification(methodDidChangeState, func(params json.RawMessage) error {
		svc.didChangeState(params)
		return nil
	}); err != nil {
		return nil, err
	}
	b.OnShutdown(func() {
		svc.shutdown()
	})
	return b, nil
}

// resolveLaunch receives the optional workspace Java tool resolved by Lithe's
// Build/Run resolver. Always launch the Go server shipped for this platform.
func resolveLaunch(params json.RawMessage, _ *lithe.Context) (any, error) {
	var p struct {
		StateDirectory string
		Platform       struct{ OS string }
		Tools          map[string]struct{ Home, Executable string }
	}
	if json.Unmarshal(params, &p) != nil || p.StateDirectory == "" {
		return nil, lithe.NewPluginError(errInvalidParams, "Missing launch directories.")
	}
	executable := "bin/java-navigation"
	if p.Platform.OS == "windows" {
		executable += ".exe"
	}
	args := []string{"--state-dir", p.StateDirectory}
	if java, ok := p.Tools["java"]; ok {
		home := java.Home
		if home == "" && filepath.IsAbs(java.Executable) {
			exe := java.Executable
			if real, err := filepath.EvalSymlinks(exe); err == nil {
				exe = real
			}
			home = filepath.Dir(filepath.Dir(exe))
		}
		if home != "" {
			args = append(args, "--jdk-home", home)
		}
	}
	return map[string]any{
		"executable": map[string]string{"kind": "packaged", "path": executable},
		"arguments":  args,
	}, nil
}

func (s *Service) shutdown() {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.stopCh)
	}
	regs := make([]*lithe.Subscription, 0, len(s.registrations))
	for id, reg := range s.registrations {
		regs = append(regs, reg.subscription)
		delete(s.registrations, id)
	}
	s.mu.Unlock()
	s.wg.Wait()
	for _, sub := range regs {
		_ = sub.Dispose()
	}
}

func (s *Service) syncAfterInitialize() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		for attempt := 0; attempt < syncRetryAttempts; attempt++ {
			select {
			case <-s.stopCh:
				return
			case <-time.After(syncRetryInterval):
			}
			failures, err := s.syncContexts()
			if err == nil {
				if failures > 0 {
					_ = s.handle.Diagnostics().Degraded(notificationOpName, catActionsRegistrationFailed, failures)
				}
				return
			}
			if isSessionStoppedError(err) {
				return
			}
		}
		_ = s.handle.Diagnostics().Degraded(notificationOpName, catContextsUnavailable, 1)
	}()
}

func (s *Service) didChangeState(params json.RawMessage) {
	var ev struct {
		ServerID string                 `json:"serverID"`
		State    string                 `json:"state"`
		Context  lithe.WorkspaceContext `json:"context"`
	}
	if json.Unmarshal(params, &ev) != nil || ev.ServerID != ServerID || ev.State != "running" {
		return
	}
	if id := ev.Context.ID(); id != "" {
		s.mu.Lock()
		stale, ok := s.registrations[id]
		if ok {
			delete(s.registrations, id)
		}
		s.mu.Unlock()
		if ok {
			_ = stale.subscription.Dispose()
		}
	}
	s.syncContextsReporting()
}

func (s *Service) syncContextsReporting() {
	failures, err := s.syncContexts()
	if err != nil {
		if !isSessionStoppedError(err) {
			_ = s.handle.Diagnostics().Degraded(notificationOpName, catContextsUnavailable, 1)
		}
		return
	}
	if failures > 0 {
		_ = s.handle.Diagnostics().Degraded(notificationOpName, catActionsRegistrationFailed, failures)
	}
}

func (s *Service) syncContexts() (uint64, error) {
	host := s.handle.Host()
	raw, err := host.Workspace().ListContexts()
	if err != nil {
		return 0, err
	}
	var listed struct {
		Contexts []json.RawMessage `json:"contexts"`
	}
	if json.Unmarshal(raw, &listed) != nil {
		return 0, errors.New("invalid contexts payload")
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return 0, nil
	}
	seen := map[string]bool{}
	var toDispose []*lithe.Subscription
	var toRegister []lithe.WorkspaceContext
	for _, entry := range listed.Contexts {
		var ref lithe.WorkspaceContext
		if json.Unmarshal(entry, &ref) != nil {
			continue
		}
		id := ref.ID()
		seen[id] = true
		if existing, ok := s.registrations[id]; ok {
			if existing.generation == ref.Generation() {
				continue
			}
			toDispose = append(toDispose, existing.subscription)
			delete(s.registrations, id)
		}
		toRegister = append(toRegister, ref)
	}
	for id, existing := range s.registrations {
		if !seen[id] {
			toDispose = append(toDispose, existing.subscription)
			delete(s.registrations, id)
		}
	}
	s.mu.Unlock()

	for _, sub := range toDispose {
		_ = sub.Dispose()
	}

	var failures uint64
	for _, ref := range toRegister {
		sub, err := host.Editor().RegisterActions(ref, map[string]any{
			"id":       ActionsBindingID,
			"function": CollectFunctionID,
			"selector": map[string]any{
				"fileExtensions": []string{"java"},
				"fileNames":      []string{},
			},
		})
		if err != nil {
			if isSessionStoppedError(err) {
				return failures, err
			}
			failures++
			continue
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = sub.Dispose()
			return failures, nil
		}
		s.registrations[ref.ID()] = registration{generation: ref.Generation(), subscription: sub}
		s.mu.Unlock()
	}
	return failures, nil
}

type lspMarkerTarget struct {
	Name string `json:"name"`
}

type lspMarkerItem struct {
	Line      int               `json:"line"`
	Character int               `json:"character"`
	Direction string            `json:"direction"`
	Relation  string            `json:"relation"`
	Targets   []lspMarkerTarget `json:"targets"`
}

func (s *Service) collect(params json.RawMessage, call *lithe.Context) (any, error) {
	if err := call.CheckCancelled(); err != nil {
		return nil, err
	}
	ref := call.WorkspaceContext()
	if ref == nil {
		return nil, lithe.NewPluginError(errContextMissing, "The call names no workspace.")
	}
	var req struct {
		Document struct {
			Path string `json:"path"`
		} `json:"document"`
		Text string `json:"text"`
	}
	if json.Unmarshal(params, &req) != nil || req.Document.Path == "" {
		return nil, lithe.NewPluginError(errInvalidParams, "The document has no path.")
	}

	root, err := workspaceRoot(call.Host(), *ref)
	if err != nil {
		return nil, err
	}
	docURI, err := documentFileURI(root, req.Document.Path)
	if err != nil {
		return nil, err
	}

	lspParams := map[string]any{
		"textDocument": map[string]any{"uri": docURI},
	}
	if req.Text != "" {
		lspParams["text"] = req.Text
	}
	raw, err := lsRequest(call.Host(), *ref, methodLSPMarkers, lspParams, collectTimeoutMs)
	if err != nil {
		if isCancelledError(err) {
			return nil, lithe.ErrCancelled
		}
		if isTransientHostError(err) {
			return map[string]any{"actions": []any{}}, nil
		}
		return nil, lithe.NewPluginError(errLanguageServerFailed, "The language server failed to collect navigation markers.")
	}

	var resp struct {
		Markers []lspMarkerItem `json:"markers"`
	}
	if len(raw) > 0 && json.Unmarshal(raw, &resp) != nil {
		return map[string]any{"actions": []any{}}, nil
	}

	semantic := supportsSemanticIcons(call.Host())
	actions := make([]map[string]any, 0, min(len(resp.Markers), maxActionsPerFile))
	for _, m := range resp.Markers {
		if len(actions) >= maxActionsPerFile {
			break
		}
		if m.Line < 0 || (m.Direction != "down" && m.Direction != "up") {
			continue
		}
		oneLine := m.Line + 1
		actions = append(actions, map[string]any{
			"id": fmt.Sprintf("nav-%s-%d", m.Direction, m.Line),
			"range": map[string]any{
				"start": map[string]any{"line": oneLine, "column": 1},
				"end":   map[string]any{"line": oneLine, "column": 1},
			},
			"title": formatMarkerTitle(m.Direction, m.Relation, m.Targets),
			"icon":  selectIcon(semantic, m.Direction, m.Relation),
			"invoke": map[string]any{
				"functionID": InvokeFunctionID,
				"params": map[string]any{
					"uri":       docURI,
					"line":      m.Line,
					"character": m.Character,
					"direction": m.Direction,
				},
			},
		})
	}
	return map[string]any{"actions": actions}, nil
}

func (s *Service) invoke(params json.RawMessage, call *lithe.Context) (any, error) {
	if err := call.CheckCancelled(); err != nil {
		return nil, err
	}
	ref := call.WorkspaceContext()
	if ref == nil {
		return nil, lithe.NewPluginError(errContextMissing, "The call names no workspace.")
	}
	var req struct {
		Data struct {
			URI       string `json:"uri"`
			Line      *int   `json:"line"`
			Character *int   `json:"character"`
			Direction string `json:"direction"`
		} `json:"data"`
	}
	if json.Unmarshal(params, &req) != nil || req.Data.URI == "" || req.Data.Line == nil || req.Data.Character == nil {
		return nil, lithe.NewPluginError(errInvalidParams, "The click carries incomplete navigation data.")
	}

	var lspMethod string
	switch req.Data.Direction {
	case "down":
		lspMethod = methodLSPImplementation
	case "up":
		lspMethod = methodLSPSuperDeclaration
	default:
		return nil, lithe.NewPluginError(errInvalidParams, "The navigation direction is invalid.")
	}

	raw, err := lsRequest(call.Host(), *ref, lspMethod, map[string]any{
		"textDocument": map[string]any{"uri": req.Data.URI},
		"position": map[string]any{
			"line":      *req.Data.Line,
			"character": *req.Data.Character,
		},
	}, invokeTimeoutMs)
	if err != nil {
		if isCancelledError(err) {
			return nil, lithe.ErrCancelled
		}
		return nil, lithe.NewPluginError(errLanguageServerFailed, "The language server failed to resolve navigation targets.")
	}

	var locations []json.RawMessage
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &locations); err != nil {
			var single json.RawMessage
			if json.Unmarshal(raw, &single) == nil && len(single) > 0 && string(single) != "null" {
				locations = []json.RawMessage{single}
			}
		}
	}
	if locations == nil {
		locations = []json.RawMessage{}
	}
	return map[string]any{
		"reveal": map[string]any{
			"locations": locations,
		},
	}, nil
}

func lsRequest(host *lithe.Host, ref lithe.WorkspaceContext, method string, params any, timeoutMs int) (json.RawMessage, error) {
	raw, err := host.Request(methodLanguageServerReq, map[string]any{
		"context":             ref,
		"serverID":            ServerID,
		"method":              method,
		"params":              params,
		"timeoutMilliseconds": timeoutMs,
	})
	if err != nil {
		return nil, err
	}
	var env struct {
		Value json.RawMessage `json:"value"`
	}
	if json.Unmarshal(raw, &env) != nil {
		return nil, nil
	}
	return env.Value, nil
}

func workspaceRoot(host *lithe.Host, ref lithe.WorkspaceContext) (string, error) {
	raw, err := host.Workspace().GetContext(ref)
	if err != nil {
		if isCancelledError(err) {
			return "", lithe.ErrCancelled
		}
		return "", lithe.NewPluginError(errWorkspaceUnavailable, "The workspace context is unavailable.")
	}
	var ctx struct {
		Folders []struct {
			Path string `json:"path"`
		} `json:"folders"`
	}
	if json.Unmarshal(raw, &ctx) != nil || len(ctx.Folders) == 0 || ctx.Folders[0].Path == "" {
		return "", lithe.NewPluginError(errWorkspaceUnavailable, "The workspace has no local folder.")
	}
	return ctx.Folders[0].Path, nil
}

func supportsSemanticIcons(host *lithe.Host) bool {
	raw, err := host.Capabilities()
	if err != nil {
		return false
	}
	var caps struct {
		Services struct {
			Editor struct {
				Options struct {
					ActionIcons []string `json:"actionIcons"`
				} `json:"options"`
			} `json:"editor"`
		} `json:"services"`
	}
	if json.Unmarshal(raw, &caps) != nil {
		return false
	}
	icons := map[string]bool{}
	for _, icon := range caps.Services.Editor.Options.ActionIcons {
		icons[icon] = true
	}
	return icons["navigate-implemented"] &&
		icons["navigate-implementing"] &&
		icons["navigate-overridden"] &&
		icons["navigate-overriding"]
}

func selectIcon(semantic bool, direction, relation string) string {
	if !semantic {
		if direction == "down" {
			return "navigate-implementations"
		}
		return "navigate-super"
	}
	switch {
	case direction == "down" && relation == "interface":
		return "navigate-implemented"
	case direction == "up" && relation == "interface":
		return "navigate-implementing"
	case direction == "down":
		return "navigate-overridden"
	default:
		return "navigate-overriding"
	}
}

func formatMarkerTitle(direction, relation string, targets []lspMarkerTarget) string {
	var verb string
	switch {
	case direction == "down" && relation == "interface":
		verb = "Implemented by"
	case direction == "down":
		verb = "Overridden by"
	case direction == "up" && relation == "interface":
		verb = "Implements"
	default:
		verb = "Overrides"
	}
	var labels []string
	seen := map[string]bool{}
	for _, t := range targets {
		name := strings.TrimSpace(t.Name)
		if name != "" && !seen[name] {
			seen[name] = true
			labels = append(labels, name)
		}
	}
	switch len(labels) {
	case 0:
		return fmt.Sprintf("%s (%d)", verb, len(targets))
	case 1, 2, 3:
		return verb + " " + strings.Join(labels, ", ")
	default:
		return fmt.Sprintf("%s %s (+%d)", verb, strings.Join(labels[:2], ", "), len(labels)-2)
	}
}

// documentFileURI converts a workspace root folder and workspace-relative
// slash path into a canonical file:// URI for both POSIX and Windows paths.
func documentFileURI(root, rel string) (string, error) {
	cleanRel := strings.TrimLeft(strings.ReplaceAll(rel, "\\", "/"), "/")
	if cleanRel == "" {
		return "", lithe.NewPluginError(errInvalidParams, "The document path is empty.")
	}
	slashRoot := strings.ReplaceAll(root, "\\", "/")
	if strings.HasPrefix(slashRoot, "//") {
		trimmed := strings.TrimPrefix(slashRoot, "//")
		host, rest, ok := strings.Cut(trimmed, "/")
		if !ok || host == "" {
			return "", lithe.NewPluginError(errWorkspaceUnavailable, "Invalid UNC workspace root.")
		}
		p := path.Clean("/" + rest + "/" + cleanRel)
		return (&url.URL{Scheme: "file", Host: host, Path: p}).String(), nil
	}
	if len(slashRoot) >= 2 && slashRoot[1] == ':' && ((slashRoot[0] >= 'A' && slashRoot[0] <= 'Z') || (slashRoot[0] >= 'a' && slashRoot[0] <= 'z')) {
		drive := strings.ToUpper(slashRoot[:1]) + ":"
		rest := strings.TrimPrefix(slashRoot[2:], "/")
		joined := path.Clean("/" + rest + "/" + cleanRel)
		return (&url.URL{Scheme: "file", Path: "/" + drive + joined}).String(), nil
	}
	if !strings.HasPrefix(slashRoot, "/") {
		return "", lithe.NewPluginError(errWorkspaceUnavailable, "Workspace root must be an absolute path.")
	}
	joined := path.Clean(slashRoot + "/" + cleanRel)
	return (&url.URL{Scheme: "file", Path: joined}).String(), nil
}

func isCancelledError(err error) bool {
	if errors.Is(err, lithe.ErrCancelled) {
		return true
	}
	var sdkErr *lithe.SdkError
	return errors.As(err, &sdkErr) && sdkErr.Code() == lithe.CodeCallCancelled
}

func isSessionStoppedError(err error) bool {
	var sdkErr *lithe.SdkError
	return errors.As(err, &sdkErr) && (sdkErr.Code() == lithe.CodeSessionClosed || sdkErr.Code() == "sdk.session.notReady")
}

func isTransientHostError(err error) bool {
	var sdkErr *lithe.SdkError
	if !errors.As(err, &sdkErr) {
		return false
	}
	switch sdkErr.Code() {
	case "plugin.host.languageServerUnavailable",
		"plugin.host.busy",
		"plugin.host.timeout",
		"plugin.host.cancelled",
		"plugin.host.languageServerError":
		return true
	default:
		return false
	}
}
