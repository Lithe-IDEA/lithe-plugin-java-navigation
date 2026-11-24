package lithe

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// FunctionHandler serves one plugin function. params is the call's params
// object, {} when the host sent none. The result is any JSON-serializable
// value, at most 1 MiB encoded. Return a [*PluginError] to answer with the
// plugin's own code, [ErrCancelled] (or an error wrapping it) when the call
// stopped because it was cancelled; any other error answers as an internal
// failure whose text stays in the backend.
type FunctionHandler func(params json.RawMessage, call *Context) (any, error)

// ViewHandler serves a view function; see [Backend.View].
type ViewHandler func(event *ViewEvent, call *Context) (ViewResult, error)

// KeymapImportHandler serves one shortcut import provider; see
// [Backend.KeymapImportProvider].
type KeymapImportHandler func(request KeymapImportRequest, call *Context) (any, error)

// CommandHandler handles a run of one of the plugin's commands, with the
// workspace context it ran in, nil when it has none.
type CommandHandler func(commandID string, context *WorkspaceContext) error

// NotificationHandler handles a host notification's params.
type NotificationHandler func(params json.RawMessage) error

// phase: idle -> listening -> initializing -> ready -> shuttingDown -> exited.
type phase int

const (
	phaseIdle phase = iota
	phaseListening
	phaseInitializing
	phaseReady
	phaseShuttingDown
	phaseExited
)

type running struct {
	functionID   string
	cancellation *Cancellation
}

type state struct {
	phase                   phase
	functions               map[string]FunctionHandler
	keymapProviders         map[string]KeymapImportHandler
	selectedKeymapProviders []string
	logging                 bool
	loggingReady            bool
	earlyDiagnostics        []map[string]any
	quietSuccess            map[string]bool
	// functionsNegotiated is true when the host offered plugin functions,
	// version FunctionsVersion.
	functionsNegotiated bool
	// incoming are the host requests being handled, by JSON-RPC ID.
	incoming map[string]*running
	// outgoing are the requests this backend sent to the host, by ID.
	outgoing map[string]*outgoing
	channel  channel
	// slotSignal is closed, and replaced, when a host-request slot frees or
	// a waiting request must look again.
	slotSignal chan struct{}
}

// shared is everything handler goroutines share with the session. Locks are
// taken in the order registrations, capabilitiesMu, mu, outputMu, never the
// other way round.
type shared struct {
	name  string
	mu    sync.Mutex
	state state

	outputMu sync.Mutex
	output   *bufio.Writer

	// registrations serializes register and unregister so the host sees
	// them in call order.
	registrations sync.Mutex
	nextRequest   atomic.Uint64

	// capabilities caches the session's getCapabilities answer.
	capabilitiesMu sync.Mutex
	capabilities   any
}

// notifySlots wakes every request waiting for a slot. The caller holds mu.
func (s *shared) notifySlots() {
	close(s.state.slotSignal)
	s.state.slotSignal = make(chan struct{})
}

func (s *shared) currentPhase() phase {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.phase
}

func (s *shared) functionsNegotiated() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.functionsNegotiated
}

func (s *shared) runningCancellations() []*Cancellation {
	tokens := make([]*Cancellation, 0, len(s.state.incoming))
	for _, call := range s.state.incoming {
		tokens = append(tokens, call.cancellation)
	}
	return tokens
}

// requireFunctions checks, with mu held, that functions can be registered.
func (s *shared) requireFunctions() error {
	switch {
	case s.state.phase == phaseExited:
		return closedError()
	case s.state.phase == phaseReady && s.state.functionsNegotiated:
		return nil
	default:
		return newSdkError("sdk.functions.unavailable", "The host has not negotiated plugin functions.")
	}
}

// write sends one message. After a failed write every later one is
// dropped: the protocol pipe is gone and the host owns the process's end.
func (s *shared) write(message any) {
	body, err := encodeJSON(message)
	if err != nil {
		return
	}
	s.outputMu.Lock()
	defer s.outputMu.Unlock()
	if s.output == nil {
		return
	}
	if writeFrame(s.output, body) != nil {
		s.output = nil
	}
}

// answer is a JSON-RPC result, or an error object when err is set.
type answer struct {
	result any
	err    map[string]any
}

func rpcError(code int, message string) answer {
	return answer{err: map[string]any{"code": code, "message": message}}
}

func (s *shared) respond(id any, a answer) {
	if a.err != nil {
		s.write(map[string]any{"jsonrpc": "2.0", "id": id, "error": a.err})
		return
	}
	s.write(map[string]any{"jsonrpc": "2.0", "id": id, "result": a.result})
}

func (s *shared) failed(operation, code string) {
	s.write(map[string]any{"jsonrpc": "2.0", "method": methodLog, "params": map[string]any{
		"kind": "operationFailed", "operation": operation, "code": code, "durationMs": 0,
	}})
}

func (s *shared) finish(code int) int {
	s.mu.Lock()
	s.state.phase = phaseExited
	s.closeChannel()
	tokens := s.runningCancellations()
	s.mu.Unlock()
	for _, token := range tokens {
		token.Cancel()
	}
	s.outputMu.Lock()
	s.output = nil
	s.outputMu.Unlock()
	return code
}

// notifyHandler runs a notification handler off the reading goroutine; a
// failure is logged with its category only.
func (s *shared) notifyHandler(handler func() error) {
	go func() {
		defer func() {
			if recover() != nil {
				s.failed("notification", "panic")
			}
		}()
		if err := handler(); err != nil {
			if kind, _ := classify(err); kind != kindCancelled {
				s.failed("notification", failureKind(err))
			}
		}
	}()
}

// request sends a lithe/function/* request; it takes no host-request slot.
func (s *shared) request(method string, params any) (any, error) {
	return s.exchange(method, params, false, nil, nil)
}

// Backend is a plugin backend. Declare functions and hooks, then call
// [Backend.Listen]:
//
//	backend := lithe.NewBackend("example-backend").Version("1.0.0")
//	err := backend.Function("snapshot.refresh", func(params json.RawMessage, call *lithe.Context) (any, error) {
//		call.ProgressPercent("Refreshing", 10)
//		var request struct{ Name *string }
//		if json.Unmarshal(params, &request) != nil || request.Name == nil {
//			return nil, lithe.NewPluginError("example.invalidName", `"name" must be a string.`)
//		}
//		return map[string]any{"refreshed": *request.Name}, nil
//	})
//	if err != nil {
//		log.Fatal(err)
//	}
//	os.Exit(backend.Listen())
//
// Declare everything before Listen; a Backend is not safe for concurrent
// declaration. After that, use [Backend.Handle] from any goroutine.
type Backend struct {
	shared            *shared
	version           string
	hasVersion        bool
	initializeHooks   []func(*Initialize) error
	shutdownHooks     []func()
	commands          map[string]CommandHandler
	contextsChanged   NotificationHandler
	hostNotifications map[string]NotificationHandler
}

// NewBackend returns a backend; name is reported as serverInfo.name.
func NewBackend(name string) *Backend {
	s := &shared{name: name}
	s.state = state{
		phase:           phaseIdle,
		functions:       map[string]FunctionHandler{},
		keymapProviders: map[string]KeymapImportHandler{},
		quietSuccess:    map[string]bool{},
		incoming:        map[string]*running{},
		outgoing:        map[string]*outgoing{},
		channel:         channel{routes: map[string]*route{}},
		slotSignal:      make(chan struct{}),
	}
	return &Backend{
		shared:            s,
		commands:          map[string]CommandHandler{},
		hostNotifications: map[string]NotificationHandler{},
	}
}

// Version sets serverInfo.version.
func (b *Backend) Version(version string) *Backend {
	b.version, b.hasVersion = version, true
	return b
}

// SuppressSuccessDiagnostics omits the successful terminal diagnostics of a
// registered high-frequency operation; failures stay visible.
func (b *Backend) SuppressSuccessDiagnostics(operation string) error {
	s := b.shared
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, registered := s.state.functions[operation]; !registered && !slices.Contains(operations, operation) {
		return newSdkError("sdk.logging.invalidOperation", "Suppress a registered operation.")
	}
	s.state.quietSuccess[operation] = true
	return nil
}

// Function declares a function offered from the start, in the initialize
// answer. To add one later, use [BackendHandle.RegisterFunction].
//
// Each call runs on a goroutine of its own, so a handler may block; it
// should stop early when its [Context.Cancellation] fires.
func (b *Backend) Function(id string, handler FunctionHandler) error {
	s := b.shared
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := checkNewFunction(id, s.state.functions); err != nil {
		return err
	}
	s.state.functions[id] = handler
	return nil
}

// View declares the view function a tool window or settings region names in
// view.function: a plugin function whose params the SDK reads into a
// [ViewEvent] and whose [ViewResult] it answers. Params that are not a view
// event fail the call before the handler runs.
func (b *Backend) View(id string, handler ViewHandler) error {
	return b.Function(id, func(params json.RawMessage, call *Context) (any, error) {
		event, err := ParseViewEvent(params)
		if err != nil {
			return nil, err
		}
		result, err := handler(event, call)
		if err != nil {
			return nil, err
		}
		return result, nil
	})
}

// KeymapImportProvider declares a shortcut import provider: id is 1-64
// lowercase letters, digits or "-", starting with a letter; a backend has at
// most 8. The handler receives an opaque snapshot, never a filesystem path;
// it lists and reads the selected inputs through its call's host.
func (b *Backend) KeymapImportProvider(id string, handler KeymapImportHandler) error {
	if !validKeymapProviderID(id) {
		return newSdkError("sdk.keymap.invalidID", "Invalid shortcut import provider ID.")
	}
	s := b.shared
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.state.keymapProviders[id]; exists || len(s.state.keymapProviders) >= 8 {
		return newSdkError("sdk.keymap.invalidRegistration", "Duplicate provider or provider limit exceeded.")
	}
	s.state.keymapProviders[id] = handler
	return nil
}

// OnInitialize runs hook before the initialize answer. A hook may read the
// host's params and add functions; an error refuses initialize, mapped like
// a handler's.
func (b *Backend) OnInitialize(hook func(*Initialize) error) {
	b.initializeHooks = append(b.initializeHooks, hook)
}

// OnShutdown runs hook on shutdown, after running calls were cancelled.
func (b *Backend) OnShutdown(hook func()) {
	b.shutdownHooks = append(b.shutdownHooks, hook)
}

// OnCommand handles lithe/host/commands/didRun for one of the plugin's
// commands, its ID from the manifest (1-128 of [a-z0-9.-]). It is an event:
// the host waits for no result, and a failure is only logged. Each run has
// a goroutine of its own. A later handler for the same ID replaces the
// earlier one.
func (b *Backend) OnCommand(commandID string, handler CommandHandler) error {
	if !IsCommandID(commandID) {
		return newSdkError("sdk.command.invalidID", fmt.Sprintf("Command ID %q must be 1-128 of [a-z0-9.-].", commandID))
	}
	b.commands[commandID] = handler
	return nil
}

// OnContextsChanged handles lithe/host/workspace/didChangeContexts with
// { revision }; call Host().Workspace().ListContexts() for the contexts.
func (b *Backend) OnContextsChanged(handler NotificationHandler) {
	b.contextsChanged = handler
}

// OnHostNotification handles a lithe/host/* notification a later host adds.
// Commands, context changes and subscription events have their own APIs.
func (b *Backend) OnHostNotification(method string, handler NotificationHandler) error {
	if !strings.HasPrefix(method, hostPrefix) {
		return newSdkError("sdk.method.invalid", method+" is not a host notification.")
	}
	if method == methodContexts || method == methodNotify || method == methodCommandRan {
		return newSdkError("sdk.method.reserved", method+" is routed by the SDK; use OnCommand, OnContextsChanged, or Host.Subscribe.")
	}
	b.hostNotifications[method] = handler
	return nil
}

// Handle returns a handle for other goroutines: runtime registration, host
// services and diagnostics.
func (b *Backend) Handle() *BackendHandle {
	return &BackendHandle{shared: b.shared}
}

// Listen serves the host on stdin and stdout until it sends exit or closes
// stdin, and returns the process exit code. Stdout carries only protocol
// frames: on Unix the SDK moves the protocol to a duplicate of the stdout
// descriptor and points stdout at stderr, so a stray fmt.Println, in the
// plugin or a dependency, lands in the host's log instead of corrupting the
// stream. On Windows plugin code must not write to stdout.
func (b *Backend) Listen() int {
	return b.Serve(os.Stdin, protocolOutput())
}

// Serve serves the host on the given streams; [Backend.Listen] uses stdin
// and stdout.
func (b *Backend) Serve(input io.Reader, output io.Writer) int {
	s := b.shared
	s.mu.Lock()
	s.state.phase = phaseListening
	s.mu.Unlock()
	s.outputMu.Lock()
	s.output = bufio.NewWriter(output)
	s.outputMu.Unlock()
	reader := bufio.NewReader(input)
	for {
		message, err := readFrame(reader)
		switch {
		case err != nil:
			b.Handle().Diagnostics().terminal("notification", errors.New("framing_error"), time.Now())
			return s.finish(1)
		case message == nil:
			code := 0
			if s.currentPhase() != phaseShuttingDown {
				code = 1
			}
			return s.finish(code)
		case message.malformed:
			s.respond(nil, rpcError(rpcParseError, "The message is not valid JSON."))
		default:
			if code, exit := b.dispatch(message.value); exit {
				return s.finish(code)
			}
		}
	}
}

// dispatch handles one message; it reports the exit code once the host
// sent exit.
func (b *Backend) dispatch(message any) (int, bool) {
	s := b.shared
	object, ok := message.(map[string]any)
	if !ok {
		// Valid JSON that is not a message object, such as null or a batch.
		s.respond(nil, rpcError(rpcInvalidRequest, "The message is not a JSON-RPC object."))
		return 0, false
	}
	id := object["id"]
	params := object["params"]
	method, isMethod := object["method"].(string)
	switch {
	case !isMethod && id != nil:
		s.settleOutgoing(id, object)
	case !isMethod:
	case id != nil:
		b.handleRequest(id, method, params)
	default:
		return b.handleNotification(method, params)
	}
	return 0, false
}

func (b *Backend) handleRequest(id any, method string, params any) {
	s := b.shared
	switch method {
	case methodInitialize:
		b.initialize(id, params)
		return
	case methodShutdown:
		b.shutdown(id)
		return
	}
	switch s.currentPhase() {
	case phaseReady:
		switch method {
		case methodInvoke:
			s.invoke(id, params)
		case methodKeymapImport:
			s.invokeKeymap(id, params)
		default:
			s.respond(id, rpcError(rpcMethodNotFound, "Unsupported method: "+method))
		}
	case phaseIdle, phaseListening, phaseInitializing:
		s.respond(id, rpcError(rpcServerNotInitialized, "The backend is not initialized."))
	default:
		s.respond(id, rpcError(rpcInvalidRequest, "The backend is shutting down."))
	}
}

func (b *Backend) handleNotification(method string, params any) (int, bool) {
	s := b.shared
	switch {
	case method == methodExit:
		if s.currentPhase() != phaseShuttingDown {
			return 1, true
		}
		return 0, true
	case method == methodCancelRequest:
		key := requestKey(field(params, "id"))
		s.mu.Lock()
		call := s.state.incoming[key]
		s.mu.Unlock()
		if call != nil {
			call.cancellation.Cancel()
		}
	case strings.HasPrefix(method, hostPrefix):
		b.hostNotification(method, params)
	}
	// initialized and anything else a plugin backend is not sent.
	return 0, false
}

// hostNotification routes a host notification. Core checked its shape and
// the session's grant; one this SDK does not know is ignored, as the
// contract asks.
func (b *Backend) hostNotification(method string, params any) {
	s := b.shared
	object, ok := params.(map[string]any)
	if !s.functionsNegotiated() || !ok {
		return
	}
	switch method {
	case methodNotify:
		s.deliver(object)
	case methodCommandRan:
		commandID, _ := object["commandID"].(string)
		handler := b.commands[commandID]
		if handler == nil {
			s.failed("notification", "command_unhandled")
			return
		}
		context := workspaceContextFromReference(object["context"])
		s.notifyHandler(func() error { return handler(commandID, context) })
	default:
		handler := b.hostNotifications[method]
		if method == methodContexts {
			handler = b.contextsChanged
		}
		if handler != nil {
			data := raw(params)
			s.notifyHandler(func() error { return handler(data) })
		}
	}
}

// runHook runs one initialize hook; it reports the error answer when the
// hook refused or panicked.
func runHook(hook func(*Initialize) error, initialize *Initialize) (refusal map[string]any) {
	defer func() {
		if recover() != nil {
			refusal = internalResponse()
		}
	}()
	if err := hook(initialize); err != nil {
		return responseError(err)
	}
	return nil
}

func (b *Backend) initialize(id, params any) {
	s := b.shared
	s.mu.Lock()
	if s.state.phase != phaseListening {
		s.mu.Unlock()
		s.respond(id, rpcError(rpcInvalidRequest, "initialize was already received."))
		return
	}
	s.state.phase = phaseInitializing
	s.mu.Unlock()

	experimentalOffer := pointer(params, "capabilities", "experimental")
	keymapOffer, hasKeymapOffer := field(experimentalOffer, "litheKeymapImport"), false
	if object, ok := experimentalOffer.(map[string]any); ok {
		_, hasKeymapOffer = object["litheKeymapImport"]
	}
	logging := offers(field(experimentalOffer, "litheLogging"), LoggingVersion)
	s.mu.Lock()
	s.state.logging = logging
	s.state.loggingReady = false
	s.state.earlyDiagnostics = nil
	if !logging {
		s.state.phase = phaseListening
	}
	existing := sortedKeys(s.state.functions)
	s.mu.Unlock()
	if !logging {
		s.respond(id, rpcError(rpcInvalidRequest, fmt.Sprintf("The backend requires litheLogging version %d.", LoggingVersion)))
		return
	}
	functions := offers(field(experimentalOffer, "litheFunctions"), FunctionsVersion)
	init := &Initialize{params: params, functions: functions, existing: existing, added: map[string]FunctionHandler{}}
	for _, hook := range b.initializeHooks {
		if refusal := runHook(hook, init); refusal != nil {
			s.mu.Lock()
			s.state.phase = phaseListening
			s.mu.Unlock()
			s.respond(id, answer{err: refusal})
			return
		}
	}

	// The state stays locked until the answer is written, so a function
	// registered from another goroutine cannot overtake it.
	s.mu.Lock()
	for functionID, handler := range init.added {
		s.state.functions[functionID] = handler
	}
	s.state.functionsNegotiated = functions
	experimental := map[string]any{"litheLogging": map[string]any{"version": LoggingVersion}}
	if hasKeymapOffer && offers(keymapOffer, KeymapImportVersion) {
		if providers, ok := field(keymapOffer, "providers").([]any); ok && len(providers) <= 8 {
			var selected []string
			for _, value := range providers {
				providerID, ok := value.(string)
				if _, registered := s.state.keymapProviders[providerID]; ok && registered && !slices.Contains(selected, providerID) {
					selected = append(selected, providerID)
				}
			}
			slices.Sort(selected)
			s.state.selectedKeymapProviders = selected
			if len(selected) > 0 {
				experimental["litheKeymapImport"] = map[string]any{"version": KeymapImportVersion, "providers": selected}
			}
		}
	}
	if functions {
		declared := []any{}
		for _, functionID := range sortedKeys(s.state.functions) {
			declared = append(declared, map[string]any{"id": functionID})
		}
		experimental["litheFunctions"] = map[string]any{"version": FunctionsVersion, "functions": declared}
	}
	serverInfo := map[string]any{"name": s.name}
	if b.hasVersion {
		serverInfo["version"] = b.version
	}
	s.state.phase = phaseReady
	s.respond(id, answer{result: map[string]any{
		"capabilities": map[string]any{"experimental": experimental},
		"serverInfo":   serverInfo,
	}})
	s.state.loggingReady = true
	early := s.state.earlyDiagnostics
	s.state.earlyDiagnostics = nil
	s.mu.Unlock()
	for _, params := range early {
		s.write(map[string]any{"jsonrpc": "2.0", "method": methodLog, "params": params})
	}
}

func (b *Backend) shutdown(id any) {
	s := b.shared
	s.mu.Lock()
	switch s.state.phase {
	case phaseListening, phaseInitializing, phaseReady:
	default:
		s.mu.Unlock()
		s.respond(id, rpcError(rpcInvalidRequest, "shutdown was already received."))
		return
	}
	s.state.phase = phaseShuttingDown
	tokens := s.runningCancellations()
	s.mu.Unlock()
	for _, token := range tokens {
		token.Cancel()
	}
	for _, hook := range b.shutdownHooks {
		func() {
			defer func() {
				if recover() != nil {
					s.failed("shutdown", "panic")
				}
			}()
			hook()
		}()
	}
	s.respond(id, answer{result: nil})
}

// outcome is how a handler ended.
type outcome struct {
	result   any
	err      error
	panicked bool
}

func runCall(handler func() (any, error)) (ended outcome) {
	defer func() {
		if recover() != nil {
			ended = outcome{panicked: true}
		}
	}()
	result, err := handler()
	return outcome{result: result, err: err}
}

func (s *shared) invoke(id, params any) {
	functionID, _ := field(params, "functionID").(string)
	operationID := field(params, "operationID")
	// What nested cross-plugin calls name as the call they belong to.
	parent, _ := operationID.(string)
	if !isParentOperationID(parent) {
		parent = ""
	}
	callParams := field(params, "params")
	if callParams == nil {
		callParams = map[string]any{}
	}
	key := requestKey(id)
	cancellation := NewCancellation()
	var (
		handler          FunctionHandler
		workspaceContext *WorkspaceContext
		caller           *Caller
	)
	s.mu.Lock()
	if s.state.functionsNegotiated {
		workspaceContext = workspaceContextFromReference(field(params, "context"))
		caller = callerFromValue(field(params, "caller"))
		handler = s.state.functions[functionID]
	}
	if handler != nil {
		s.state.incoming[key] = &running{functionID: functionID, cancellation: cancellation}
	}
	s.mu.Unlock()
	if handler == nil {
		s.respond(id, rpcError(rpcInvalidParams, fmt.Sprintf("Function %s is not registered.", functionID)))
		return
	}
	go func() {
		// Ends host requests the call left behind, on copies of its host.
		ended := NewCancellation()
		call := &Context{
			shared:                s,
			functionID:            functionID,
			operationID:           operationID,
			diagnosticOperationID: requestOperationID(operationID, id),
			cancellation:          cancellation,
			host:                  &Host{shared: s, scope: []*Cancellation{cancellation, ended}, parentOperation: parent},
			workspaceContext:      workspaceContext,
			caller:                caller,
		}
		call.reporting.Store(true)
		started := time.Now()
		data := raw(callParams)
		result := runCall(func() (any, error) { return handler(data, call) })
		call.reporting.Store(false)
		ended.Cancel()
		a := s.functionAnswer(call, functionID, result, started)
		s.mu.Lock()
		delete(s.state.incoming, key)
		s.mu.Unlock()
		s.respond(id, a)
	}()
}

func (s *shared) invokeKeymap(id, params any) {
	object, isObject := params.(map[string]any)
	provider, _ := object["providerID"].(string)
	snapshot, _ := object["snapshotID"].(string)
	if !isObject || len(object) != 2 || !validKeymapProviderID(provider) || !validSnapshotID(snapshot) {
		s.respond(id, rpcError(rpcInvalidParams, "Invalid shortcut import request."))
		return
	}
	callID := "keymap:" + provider
	key := requestKey(id)
	cancellation := NewCancellation()
	var handler KeymapImportHandler
	s.mu.Lock()
	if slices.Contains(s.state.selectedKeymapProviders, provider) {
		handler = s.state.keymapProviders[provider]
	}
	if handler != nil {
		s.state.incoming[key] = &running{functionID: callID, cancellation: cancellation}
	}
	s.mu.Unlock()
	if handler == nil {
		s.respond(id, rpcError(rpcInvalidParams, "Shortcut import provider was not negotiated."))
		return
	}
	go func() {
		ended := NewCancellation()
		call := &Context{
			shared:                s,
			functionID:            callID,
			diagnosticOperationID: requestOperationID(nil, id),
			cancellation:          cancellation,
			host:                  &Host{shared: s, scope: []*Cancellation{cancellation, ended}},
		}
		started := time.Now()
		request := KeymapImportRequest{providerID: provider, snapshotID: snapshot}
		result := runCall(func() (any, error) { return handler(request, call) })
		ended.Cancel()
		// Facts name the wire method Core registered, not the per-provider call ID.
		a := s.functionAnswer(call, methodKeymapImport, result, started)
		s.mu.Lock()
		delete(s.state.incoming, key)
		s.mu.Unlock()
		s.respond(id, a)
	}()
}

// functionAnswer is the answer to one call. A cancelled call answers as
// cancelled whatever the handler returned, and failures the host never sees
// in full are logged with a fixed category, without their private contents.
func (s *shared) functionAnswer(call *Context, function string, ended outcome, started time.Time) answer {
	diagnostics := call.Diagnostics()
	switch {
	case call.cancellation.IsCancelled():
		return answer{err: cancelledResponse()}
	case ended.panicked:
		code, duration := "panic", elapsed(started)
		_ = diagnostics.record("operationFailed", function, &code, nil, &duration)
		return answer{err: internalResponse()}
	case ended.err != nil:
		if kind, _ := classify(ended.err); kind != kindCancelled {
			diagnostics.terminal(function, ended.err, started)
		}
		return answer{err: responseError(ended.err)}
	}
	body, err := encodeJSON(ended.result)
	if err != nil || len(body) > maxResultBytes {
		diagnostics.terminal(function, errors.New("result_limit"), started)
		return answer{err: internalResponse()}
	}
	quiet := call.quietSuccess.Load()
	if !quiet {
		s.mu.Lock()
		quiet = s.state.quietSuccess[function]
		s.mu.Unlock()
	}
	if !quiet {
		diagnostics.terminal(function, nil, started)
	}
	return answer{result: json.RawMessage(body)}
}

func checkNewFunction(id string, functions map[string]FunctionHandler) error {
	if !IsFunctionID(id) {
		return newSdkError("sdk.function.invalidID", fmt.Sprintf("Function ID %q must be dot-separated lowerCamelCase segments of at most 128 bytes.", id))
	}
	if _, exists := functions[id]; exists {
		return newSdkError("sdk.function.duplicate", fmt.Sprintf("Function %s is already declared.", id))
	}
	if len(functions) >= MaxFunctions {
		return newSdkError("sdk.function.limit", fmt.Sprintf("A backend serves at most %d functions.", MaxFunctions))
	}
	return nil
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// Initialize is what a hook sees of initialize, before the backend answers
// it.
type Initialize struct {
	params    any
	functions bool
	existing  []string
	added     map[string]FunctionHandler
}

// Params are the host's initialize params.
func (i *Initialize) Params() json.RawMessage { return raw(i.params) }

// FunctionsVersion is the plugin-function protocol version the backend will
// answer, with false when the host did not offer it.
func (i *Initialize) FunctionsVersion() (uint64, bool) {
	return FunctionsVersion, i.functions
}

// RootPath is the rootUri as a path. For a plugin backend this is the
// plugin's ${stateDir}, also the working directory.
func (i *Initialize) RootPath() (string, bool) {
	uri, ok := field(i.params, "rootUri").(string)
	if !ok {
		return "", false
	}
	return fileURIPath(uri)
}

// Function adds a function to the initial list, for example one that
// depends on state found at startup.
func (i *Initialize) Function(id string, handler FunctionHandler) error {
	if slices.Contains(i.existing, id) {
		return newSdkError("sdk.function.duplicate", fmt.Sprintf("Function %s is already declared.", id))
	}
	if len(i.existing)+len(i.added) >= MaxFunctions {
		return newSdkError("sdk.function.limit", fmt.Sprintf("A backend serves at most %d functions.", MaxFunctions))
	}
	if err := checkNewFunction(id, i.added); err != nil {
		return err
	}
	i.added[id] = handler
	return nil
}

// fileURIPath is the path of a file: URI, percent-decoded.
func fileURIPath(uri string) (string, bool) {
	rest, ok := strings.CutPrefix(uri, "file://")
	if !ok {
		return "", false
	}
	path := strings.TrimPrefix(rest, "localhost")
	if !strings.HasPrefix(path, "/") {
		return "", false
	}
	decoded := make([]byte, 0, len(path))
	for index := 0; index < len(path); {
		if path[index] == '%' && index+2 < len(path) {
			value, err := strconv.ParseUint(path[index+1:index+3], 16, 8)
			if err != nil {
				return "", false
			}
			decoded = append(decoded, byte(value))
			index += 3
			continue
		}
		decoded = append(decoded, path[index])
		index++
	}
	if !utf8.Valid(decoded) {
		return "", false
	}
	result := string(decoded)
	// /C:/Users on Windows.
	if runtime.GOOS == "windows" && len(result) > 2 && result[2] == ':' {
		result = result[1:]
	}
	for len(result) > 1 && strings.HasSuffix(result, "/") {
		result = result[:len(result)-1]
	}
	return result, true
}

// Context is one running function call.
type Context struct {
	shared                *shared
	functionID            string
	operationID           any
	diagnosticOperationID string
	cancellation          *Cancellation
	host                  *Host
	workspaceContext      *WorkspaceContext
	caller                *Caller
	reporting             atomic.Bool
	quietSuccess          atomic.Bool
}

// SuppressSuccessDiagnostics omits only this call's successful terminal
// fact; failures remain visible.
func (c *Context) SuppressSuccessDiagnostics() { c.quietSuccess.Store(true) }

// Diagnostics records structured facts correlated with the host's original
// operation ID.
func (c *Context) Diagnostics() *Diagnostics {
	return &Diagnostics{shared: c.shared, operationID: c.diagnosticOperationID}
}

// FunctionID is the ID of the function being called.
func (c *Context) FunctionID() string { return c.functionID }

// Cancellation fires when the host cancels the call, withdraws its
// function, or shuts the backend down. Core has already ended the call by
// then, so a handler should stop its work and return promptly.
func (c *Context) Cancellation() *Cancellation { return c.cancellation }

// CheckCancelled returns ErrCancelled once the call is cancelled.
func (c *Context) CheckCancelled() error { return c.cancellation.Check() }

// Progress reports progress of this call; the host shows the latest report.
// message is cut to 1024 bytes. Reports after the call ended are dropped.
func (c *Context) Progress(message string) { c.progress(message, nil) }

// ProgressPercent reports progress with a percentage, clamped to 0-100.
func (c *Context) ProgressPercent(message string, percentage int) {
	percentage = max(0, min(percentage, 100))
	c.progress(message, &percentage)
}

func (c *Context) progress(message string, percentage *int) {
	if !c.reporting.Load() {
		return
	}
	report := map[string]any{
		"operationID": c.operationID,
		"message":     boundedText(message, maxErrorMessageBytes),
	}
	if percentage != nil {
		report["percentage"] = *percentage
	}
	c.shared.write(map[string]any{"jsonrpc": "2.0", "method": methodProgress, "params": report})
}

// Host is the host services of this call. A request still waiting for the
// host when the call is cancelled or ends fails with sdk.call.cancelled,
// which counts as a cancelled answer when returned; the SDK also cancels it
// on the wire.
func (c *Context) Host() *Host { return c.host }

// WorkspaceContext is the { contextID, generation } of the window the call
// came from; nil for calls from no workspace, such as a view or page shown
// without one.
func (c *Context) WorkspaceContext() *WorkspaceContext { return c.workspaceContext }

// Caller is the plugin that called this function through Core's relay; nil
// when the host started the call, and for a caller of any other shape.
func (c *Context) Caller() *Caller { return c.caller }

// Credentials is Host().Credentials().
func (c *Context) Credentials() *Credentials { return c.host.Credentials() }

// Backend is a handle to the backend serving the call.
func (c *Context) Backend() *BackendHandle { return &BackendHandle{shared: c.shared} }

// Caller is the plugin that called a function through Core's relay. Core
// writes it from the identity of the calling session and a caller cannot
// supply it, so a provider may key per-caller state by it.
type Caller struct {
	pluginID string
}

// callerFromValue reads the caller of a relayed call, which is exactly
// { pluginID } with a non-empty string.
func callerFromValue(value any) *Caller {
	object, ok := value.(map[string]any)
	if !ok || len(object) != 1 {
		return nil
	}
	pluginID, ok := object["pluginID"].(string)
	if !ok || pluginID == "" {
		return nil
	}
	return &Caller{pluginID: pluginID}
}

// PluginID is the calling plugin's ID, such as dev.lithe.fixture.python.
func (c *Caller) PluginID() string { return c.pluginID }

// KeymapImportRequest is a single host-authorized source snapshot for a
// shortcut import.
type KeymapImportRequest struct {
	providerID string
	snapshotID string
}

// KeymapSourceFile is a source file in the host's selected, read-only
// snapshot. Path is relative to the selected source and only a label, never
// authority.
type KeymapSourceFile struct {
	ID        string
	Path      string
	ByteCount uint64
}

// ProviderID is the provider the host called.
func (r KeymapImportRequest) ProviderID() string { return r.providerID }

// SnapshotID names the snapshot the host captured.
func (r KeymapImportRequest) SnapshotID() string { return r.snapshotID }

// ListInputs lists the declared files captured for this request.
func (r KeymapImportRequest) ListInputs(host *Host) ([]KeymapSourceFile, error) {
	result, err := host.request(methodKeymapInputs, map[string]any{"providerID": r.providerID, "snapshotID": r.snapshotID})
	if err != nil {
		return nil, err
	}
	invalid := newSdkError("sdk.keymap.invalidResponse", "Invalid shortcut source list.")
	entries, ok := field(result, "files").([]any)
	if !ok || len(entries) > 128 {
		return nil, invalid
	}
	files := make([]KeymapSourceFile, 0, len(entries))
	for _, entry := range entries {
		id, ok := field(entry, "id").(string)
		if !ok || !validSnapshotID(id) {
			return nil, invalid
		}
		path, ok := field(entry, "path").(string)
		if !ok || path == "" || len(path) > 4096 {
			return nil, invalid
		}
		size, ok := unsignedInteger(field(entry, "byteCount"))
		if !ok || size > 1_048_576 {
			return nil, invalid
		}
		files = append(files, KeymapSourceFile{ID: id, Path: path, ByteCount: size})
	}
	return files, nil
}

// ReadChunk reads one bounded chunk, at most maximumBytes (1-131072) from
// offset (at most 1 MiB), of a file ID ListInputs returned.
func (r KeymapImportRequest) ReadChunk(host *Host, fileID string, offset uint64, maximumBytes uint32) ([]byte, error) {
	invalid := newSdkError("sdk.keymap.invalidChunk", "Invalid shortcut source chunk.")
	if !validSnapshotID(fileID) || offset > 1_048_576 || maximumBytes == 0 || maximumBytes > 131_072 {
		return nil, invalid
	}
	result, err := host.request(methodKeymapChunk, map[string]any{
		"providerID": r.providerID, "snapshotID": r.snapshotID,
		"fileID": fileID, "offset": offset, "maximumBytes": maximumBytes,
	})
	if err != nil {
		return nil, err
	}
	encoded, ok := field(result, "data").(string)
	// Standard base64 with padding and nothing else; the decoder would skip
	// line breaks.
	if !ok || len(encoded) > 174_764 || strings.ContainsAny(encoded, "\r\n") {
		return nil, invalid
	}
	data, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(data) > int(maximumBytes) {
		return nil, invalid
	}
	return data, nil
}

func validKeymapProviderID(id string) bool {
	if id == "" || len(id) > 64 || id[0] < 'a' || id[0] > 'z' {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !(c >= 'a' && c <= 'z') && !(c >= '0' && c <= '9') && c != '-' {
			return false
		}
	}
	return true
}

func validSnapshotID(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for i := 0; i < len(id); i++ {
		if !isASCIIAlnum(id[i]) && id[i] != '-' && id[i] != '_' {
			return false
		}
	}
	return true
}

// BackendHandle is a handle to a backend for any goroutine.
type BackendHandle struct {
	shared *shared
}

// Diagnostics records structured facts outside a call; no operation ID is
// fabricated.
func (h *BackendHandle) Diagnostics() *Diagnostics { return &Diagnostics{shared: h.shared} }

// FunctionsVersion is the negotiated plugin-function protocol version, with
// false before initialize or when the host did not offer it.
func (h *BackendHandle) FunctionsVersion() (uint64, bool) {
	return FunctionsVersion, h.shared.functionsNegotiated()
}

// FunctionIDs are the IDs of the functions the backend currently serves,
// sorted.
func (h *BackendHandle) FunctionIDs() []string {
	h.shared.mu.Lock()
	defer h.shared.mu.Unlock()
	return sortedKeys(h.shared.state.functions)
}

// RegisterFunction adds a function after initialize and waits until the
// host accepted it. The handler is installed first, because the host may
// invoke it as soon as its registry changed.
func (h *BackendHandle) RegisterFunction(id string, handler FunctionHandler) error {
	s := h.shared
	s.registrations.Lock()
	defer s.registrations.Unlock()
	s.mu.Lock()
	err := s.requireFunctions()
	if err == nil {
		err = checkNewFunction(id, s.state.functions)
	}
	if err != nil {
		s.mu.Unlock()
		return err
	}
	s.state.functions[id] = handler
	s.mu.Unlock()
	if _, err := s.request(methodRegister, map[string]any{"functions": []any{map[string]any{"id": id}}}); err != nil {
		s.mu.Lock()
		delete(s.state.functions, id)
		s.mu.Unlock()
		return err
	}
	return nil
}

// UnregisterFunction withdraws a function and waits until the host accepted
// it. The host ends the function's running calls as revoked, and the SDK
// cancels them. A handler must not withdraw its own function: that revokes
// the call it runs in.
func (h *BackendHandle) UnregisterFunction(id string) error {
	s := h.shared
	s.registrations.Lock()
	defer s.registrations.Unlock()
	s.mu.Lock()
	err := s.requireFunctions()
	if _, registered := s.state.functions[id]; err == nil && !registered {
		err = newSdkError("sdk.function.unknown", fmt.Sprintf("Function %s is not registered.", id))
	}
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if _, err := s.request(methodUnregister, map[string]any{"ids": []any{id}}); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.state.functions, id)
	delete(s.state.quietSuccess, id)
	var tokens []*Cancellation
	for _, call := range s.state.incoming {
		if call.functionID == id {
			tokens = append(tokens, call.cancellation)
		}
	}
	s.mu.Unlock()
	for _, token := range tokens {
		token.Cancel()
	}
	return nil
}

// Host is the host services for work outside a function call; usable once
// the session is ready.
func (h *BackendHandle) Host() *Host { return &Host{shared: h.shared} }

// Credentials is Host().Credentials().
func (h *BackendHandle) Credentials() *Credentials { return h.Host().Credentials() }
