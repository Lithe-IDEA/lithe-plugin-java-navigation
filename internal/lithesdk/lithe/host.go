package lithe

// Host services of the plugin-function protocol: the lithe/host/* requests a
// backend sends to Lithe, the subscriptions some of them create, and the
// [Host] handlers use. The backend owns the wire; this file owns what Core
// expects of a well-behaved client -- at most 16 unanswered host requests,
// cancellation through $/cancelRequest, and subscription events routed by ID.
//
// Contract: contract/v1/README.md, "Host services" and the host sections
// after it.

import (
	"encoding/json"
	"errors"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// notNegotiated is the answer Core gives a host request when it did not
// offer litheFunctions.
const notNegotiated = "The host did not negotiate plugin functions (litheFunctions), which offer host services."

// settled is how a request this backend sent ended: the host's result, or
// an error.
type settled struct {
	value any
	err   error
}

// outgoing is one request this backend sent to the host, until the host
// answers it.
type outgoing struct {
	method string
	reply  chan settled
	// slot is whether it holds one of the 16 host-request slots. A cancelled
	// request keeps its slot until the host answers.
	slot bool
	// route is where the events of the subscription it creates go.
	route *route
}

// channel is the session's host-request state, guarded by shared.mu.
type channel struct {
	inFlight int
	// waiting are the requests waiting for a free slot, in call order.
	waiting    []uint64
	nextTicket uint64
	// subscribing counts subscribing requests not yet answered; early events
	// wait only while there are some.
	subscribing int
	routes      map[string]*route
	early       []earlyEvent
}

type earlyEvent struct {
	subscriptionID string
	sequence       uint64
	event          any
}

type route struct {
	live  *atomic.Bool
	queue *eventQueue
	last  uint64
}

func (r *route) emit(subscriptionID string, sequence uint64, event any) {
	gap := r.last == math.MaxUint64 || r.last+1 != sequence
	r.last = sequence
	r.queue.push(Event{subscriptionID: subscriptionID, sequence: sequence, gap: gap, event: event})
}

// refusal is why the state refuses a new request, if it does. The caller
// holds mu.
func refusal(st *state, scope []*Cancellation) error {
	switch st.phase {
	case phaseReady:
		if anyCancelled(scope) {
			return cancelledError()
		}
		return nil
	case phaseExited:
		return closedError()
	default:
		return newSdkError("sdk.session.notReady", "The session is not initialized.")
	}
}

// exchange sends a request to the host and waits for its answer, the end of
// the session, or the cancellation of a token in scope. A request with a
// slot first waits its turn among the 16 the host accepts at once.
func (s *shared) exchange(method string, params any, slot bool, scope []*Cancellation, rt *route) (any, error) {
	id := "sdk-" + strconv.FormatUint(s.nextRequest.Add(1), 10)
	reply := make(chan settled, 1)
	abort := make(chan struct{})
	var once sync.Once
	wake := func() { once.Do(func() { close(abort) }) }
	type watched struct {
		token *Cancellation
		id    uint64
	}
	var watches []watched
	for _, token := range scope {
		if watchID, ok := token.watch(wake); ok {
			watches = append(watches, watched{token, watchID})
		}
	}
	var live *atomic.Bool
	if rt != nil {
		live = rt.live
	}
	result, cancelled := s.sendAndWait(id, method, params, slot, scope, rt, reply, abort)
	for _, watch := range watches {
		watch.token.unwatch(watch.id)
	}
	if !cancelled {
		return result.value, result.err
	}
	s.mu.Lock()
	if live != nil {
		// Answered just before the cancellation won.
		for subscriptionID, existing := range s.state.channel.routes {
			if existing.live == live {
				delete(s.state.channel.routes, subscriptionID)
				existing.queue.close()
			}
		}
	}
	entry, pending := s.state.outgoing[id]
	if pending {
		entry.route = nil
	}
	negotiated := s.state.functionsNegotiated
	s.mu.Unlock()
	if pending && negotiated {
		s.write(map[string]any{"jsonrpc": "2.0", "method": methodCancelRequest, "params": map[string]any{"id": id}})
	}
	// A cancelled mutation may already have taken effect; it is never sent
	// again.
	return nil, cancelledError()
}

// sendAndWait reports true when a token in scope was cancelled while the
// request was on the wire.
func (s *shared) sendAndWait(id, method string, params any, slot bool, scope []*Cancellation, rt *route, reply chan settled, abort chan struct{}) (settled, bool) {
	s.mu.Lock()
	if slot {
		ticket := s.state.channel.nextTicket
		s.state.channel.nextTicket++
		s.state.channel.waiting = append(s.state.channel.waiting, ticket)
		for {
			if err := refusal(&s.state, scope); err != nil {
				s.state.channel.waiting = slices.DeleteFunc(s.state.channel.waiting, func(waiting uint64) bool { return waiting == ticket })
				s.notifySlots()
				s.mu.Unlock()
				return settled{err: err}, false
			}
			if s.state.channel.inFlight < maxPendingHostCalls && s.state.channel.waiting[0] == ticket {
				break
			}
			signal := s.state.slotSignal
			s.mu.Unlock()
			select {
			case <-signal:
			case <-abort:
			}
			s.mu.Lock()
		}
		s.state.channel.waiting = s.state.channel.waiting[1:]
		s.state.channel.inFlight++
		s.notifySlots()
	} else if err := refusal(&s.state, scope); err != nil {
		s.mu.Unlock()
		return settled{err: err}, false
	}
	s.state.outgoing[id] = &outgoing{method: method, reply: reply, slot: slot, route: rt}
	s.mu.Unlock()
	message := map[string]any{"jsonrpc": "2.0", "id": id, "method": method}
	if params != nil {
		message["params"] = params
	}
	s.write(message)
	select {
	case result := <-reply:
		return result, false
	case <-abort:
		select {
		case result := <-reply:
			return result, false
		default:
			return settled{}, true
		}
	}
}

// settleOutgoing settles the request a host response answers.
func (s *shared) settleOutgoing(id any, message map[string]any) {
	key, ok := id.(string)
	if !ok {
		return
	}
	s.mu.Lock()
	entry, ok := s.state.outgoing[key]
	if !ok {
		s.mu.Unlock()
		return
	}
	delete(s.state.outgoing, key)
	if entry.slot {
		s.state.channel.inFlight--
		s.notifySlots()
	}
	var result settled
	if failure := message["error"]; failure != nil {
		result.err = sdkErrorFromResponse(entry.method, failure)
	} else {
		result.value = message["result"]
	}
	if result.err == nil && entry.route != nil {
		subscriptionID, ok := field(result.value, "subscriptionID").(string)
		if ok && IsCredentialKey(subscriptionID) {
			ch := &s.state.channel
			var rest []earlyEvent
			// Events the host sent before this answer.
			for _, early := range ch.early {
				if early.subscriptionID == subscriptionID {
					entry.route.emit(subscriptionID, early.sequence, early.event)
				} else {
					rest = append(rest, early)
				}
			}
			ch.early = rest
			if old := ch.routes[subscriptionID]; old != nil {
				old.queue.close()
			}
			ch.routes[subscriptionID] = entry.route
		} else {
			result = settled{err: newSdkError("sdk.host.invalidResult", entry.method+" answered without a subscriptionID.")}
		}
	}
	s.mu.Unlock()
	entry.reply <- result
}

// deliver routes one lithe/host/subscriptions/notify.
func (s *shared) deliver(params map[string]any) {
	subscriptionID, ok := params["subscriptionID"].(string)
	if !ok {
		return
	}
	sequence, _ := unsignedInteger(params["sequence"])
	event := params["event"]
	s.mu.Lock()
	ch := &s.state.channel
	if rt := ch.routes[subscriptionID]; rt != nil {
		rt.emit(subscriptionID, sequence, event)
	} else if ch.subscribing > 0 {
		if len(ch.early) >= maxEarlyEvents {
			s.mu.Unlock()
			s.failed("notification", "subscription_queue_full")
			return
		}
		ch.early = append(ch.early, earlyEvent{subscriptionID, sequence, event})
	}
	// Otherwise the subscription was disposed; nothing follows a disposal.
	s.mu.Unlock()
}

// closeChannel ends every host request and subscription with the session.
// The caller holds mu.
func (s *shared) closeChannel() {
	for id, entry := range s.state.outgoing {
		delete(s.state.outgoing, id)
		entry.reply <- settled{err: closedError()}
	}
	for id, rt := range s.state.channel.routes {
		delete(s.state.channel.routes, id)
		rt.live.Store(false)
		rt.queue.close()
	}
	s.state.channel.early = nil
	s.notifySlots()
}

// dispose stops routing a subscription's events at once and asks the host
// to dispose it. live limits the first step to one subscription object.
func (s *shared) dispose(subscriptionID string, live *atomic.Bool) error {
	s.mu.Lock()
	routes := s.state.channel.routes
	if rt := routes[subscriptionID]; rt != nil && (live == nil || rt.live == live) {
		delete(routes, subscriptionID)
		rt.live.Store(false)
		rt.queue.close()
	}
	ready := s.state.phase == phaseReady
	s.mu.Unlock()
	if !ready {
		return nil
	}
	_, err := (&Host{shared: s}).request(methodDispose, map[string]any{"subscriptionID": subscriptionID})
	return err
}

// Host is the host services (lithe/host/*): credentials; workspace contexts,
// files, documents, the editor, previews, the browser, messages, terminals
// and capability discovery; calls to other plugins' functions. They are
// available when the host offered litheFunctions. Every method blocks until
// the host answers; failures the host reports carry its error.data.code,
// such as plugin.host.contextExpired.
//
// [BackendHandle.Host] is usable once the session is ready. A handler's
// [Context.Host] belongs to its call: requests still unanswered when the
// call is cancelled or ends fail with sdk.call.cancelled and are cancelled
// on the wire. At most 16 requests are on the wire at once; the rest wait
// their turn.
//
// Results are JSON as the host sent it; unmarshal them into the types you
// need.
type Host struct {
	shared *shared
	scope  []*Cancellation
	// parentOperation is the operationID of the relayed call this host
	// serves, if any; the default parentOperationID of its cross-plugin
	// calls.
	parentOperation string
}

// Negotiated reports whether the host offered litheFunctions, and with it
// host services, in this session; false before initialize.
func (h *Host) Negotiated() bool { return h.shared.functionsNegotiated() }

// WithCancellation returns the same services, with requests also cancelled
// by cancellation, for work a plugin may stop for its own reasons.
func (h *Host) WithCancellation(cancellation *Cancellation) *Host {
	scope := append(slices.Clone(h.scope), cancellation)
	return &Host{shared: h.shared, scope: scope, parentOperation: h.parentOperation}
}

// Request sends any lithe/host/* request, including methods a later host
// adds; [Host.Supports] says whether the session may call it. params is
// any JSON-serializable value; nil sends no params.
func (h *Host) Request(method string, params any) (json.RawMessage, error) {
	value, err := toValue(params)
	if err != nil {
		return nil, invalidHostParams()
	}
	result, err := h.request(method, value)
	if err != nil {
		return nil, err
	}
	return raw(result), nil
}

// Subscribe sends a request that answers { subscriptionID } and routes the
// subscription's events to the returned [Subscription].
func (h *Host) Subscribe(method string, params any) (*Subscription, error) {
	value, err := toValue(params)
	if err != nil {
		return nil, invalidHostParams()
	}
	return h.subscribe(method, value)
}

// Capabilities is { protocolVersion, backendAuthority, services, limits },
// asked once per session.
func (h *Host) Capabilities() (json.RawMessage, error) {
	capabilities, err := h.capabilitiesValue()
	if err != nil {
		return nil, err
	}
	return raw(capabilities), nil
}

// Supports reports whether this session may call method, from
// [Host.Capabilities]. A missing grant still fails the request. Without
// litheFunctions nothing is supported.
func (h *Host) Supports(method string) (bool, error) {
	if !h.Negotiated() {
		return false, nil
	}
	capabilities, err := h.capabilitiesValue()
	if err != nil {
		return false, err
	}
	services, _ := field(capabilities, "services").(map[string]any)
	for _, service := range services {
		methods, _ := field(service, "methods").([]any)
		for _, entry := range methods {
			if name, ok := entry.(string); ok && name == method {
				return true, nil
			}
		}
	}
	return false, nil
}

func (h *Host) capabilitiesValue() (any, error) {
	if !h.Negotiated() {
		return nil, unavailable(notNegotiated)
	}
	s := h.shared
	// Held during the request, so concurrent callers share one answer.
	s.capabilitiesMu.Lock()
	defer s.capabilitiesMu.Unlock()
	if s.capabilities != nil {
		return s.capabilities, nil
	}
	capabilities, err := h.request(methodCapabilities, nil)
	if err != nil {
		return nil, err
	}
	if _, ok := capabilities.(map[string]any); !ok {
		return nil, newSdkError("sdk.host.invalidResult", "lithe/host/getCapabilities answered with something other than an object.")
	}
	s.capabilities = capabilities
	return capabilities, nil
}

// Credentials is the host's credentials service.
func (h *Host) Credentials() *Credentials { return &Credentials{host: h} }

// Subscriptions disposes subscriptions by ID.
func (h *Host) Subscriptions() *Subscriptions { return &Subscriptions{host: h} }

// Workspace is workspace contexts and files.
func (h *Host) Workspace() *Workspace { return &Workspace{host: h} }

// Documents is open documents, read from their live buffers.
func (h *Host) Documents() *Documents { return &Documents{host: h} }

// Editor is the text editor.
func (h *Host) Editor() *Editor { return &Editor{host: h} }

// Preview is Markdown and HTML previews.
func (h *Host) Preview() *Preview { return &Preview{host: h} }

// Browser is the system browser.
func (h *Host) Browser() *Browser { return &Browser{host: h} }

// Terminal is integrated native terminals.
func (h *Host) Terminal() *Terminal { return &Terminal{host: h} }

// Window is messages in the window.
func (h *Host) Window() *Window { return &Window{host: h} }

// Functions is calls to other plugins' functions through Core.
func (h *Host) Functions() *Functions { return &Functions{host: h} }

// request sends a host request whose params are already in the SDK's
// dynamic form.
func (h *Host) request(method string, params any) (any, error) {
	return h.send(method, params, nil)
}

func (h *Host) subscribe(method string, params any) (*Subscription, error) {
	live := &atomic.Bool{}
	live.Store(true)
	rt := &route{live: live, queue: newEventQueue()}
	s := h.shared
	s.mu.Lock()
	s.state.channel.subscribing++
	s.mu.Unlock()
	result, err := h.send(method, params, rt)
	s.mu.Lock()
	s.state.channel.subscribing--
	if s.state.channel.subscribing == 0 {
		s.state.channel.early = nil
	}
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	id, _ := field(result, "subscriptionID").(string)
	return &Subscription{id: id, shared: s, live: live, queue: rt.queue}, nil
}

func (h *Host) send(method string, params any, rt *route) (any, error) {
	if !strings.HasPrefix(method, hostPrefix) {
		return nil, newSdkError("sdk.host.method", method+" is not a host method; host requests are lithe/host/*.")
	}
	s := h.shared
	s.mu.Lock()
	err := refusal(&s.state, nil)
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if !h.Negotiated() {
		// The answer Core would give, without the round trip.
		return nil, unavailable(notNegotiated)
	}
	if anyCancelled(h.scope) {
		return nil, cancelledError()
	}
	return s.exchange(method, params, true, h.scope, rt)
}

// scoped sends a method whose params name a workspace context; it answers
// an object or null.
func (h *Host) scoped(method string, context WorkspaceContext, params any) (json.RawMessage, error) {
	value, err := toValue(params)
	if err != nil {
		return nil, invalidHostParams()
	}
	if value, err = withContext(context, value); err != nil {
		return nil, err
	}
	return h.objectOrNull(method, value)
}

func (h *Host) objectOrNull(method string, params any) (json.RawMessage, error) {
	result, err := h.request(method, params)
	if err != nil {
		return nil, err
	}
	switch result.(type) {
	case nil, map[string]any:
		return raw(result), nil
	}
	return nil, invalidResult(method)
}

// requestObject sends a method that always answers an object.
func (h *Host) requestObject(method string, params any) (json.RawMessage, error) {
	result, err := h.request(method, params)
	if err != nil {
		return nil, err
	}
	if _, ok := result.(map[string]any); !ok {
		return nil, invalidResult(method)
	}
	return raw(result), nil
}

func invalidResult(method string) *SdkError {
	return newSdkError("sdk.host.invalidResult", method+" answered with something other than an object.")
}

func invalidHostParams() *SdkError {
	return newSdkError("sdk.host.invalidParams", "Host request params are an object.")
}

// withContext is params with context added; params are an object or nil.
func withContext(context WorkspaceContext, params any) (any, error) {
	var object map[string]any
	switch value := params.(type) {
	case nil:
		object = map[string]any{}
	case map[string]any:
		object = maps.Clone(value)
	default:
		return nil, invalidHostParams()
	}
	object["context"] = context.toValue()
	return object, nil
}

// WorkspaceContext is a workspace context reference, { contextID,
// generation }: the window a call or command ran in, or one from
// ListContexts. A context is replaced when its window's folders change;
// requests with an old generation fail with plugin.host.contextExpired.
type WorkspaceContext struct {
	contextID  string
	generation uint64
}

func invalidContext() *SdkError {
	return newSdkError("sdk.host.invalidContext", "A context is { contextID, generation }: an ID of 1-128 ASCII letters, digits, \".\", \"_\", \":\" or \"-\", and a positive generation.")
}

// NewWorkspaceContext returns a context reference. contextID is 1-128 ASCII
// letters, digits, ".", "_", ":" or "-"; generation is positive.
func NewWorkspaceContext(contextID string, generation uint64) (WorkspaceContext, error) {
	return ParseWorkspaceContext(map[string]any{"contextID": contextID, "generation": generation})
}

// ParseWorkspaceContext reads the reference of a context reference or of a
// full context from ListContexts or GetContext -- a json.RawMessage, a map,
// or any value that serializes to one -- so either can be passed on.
func ParseWorkspaceContext(value any) (WorkspaceContext, error) {
	decoded, err := toValue(value)
	if err != nil {
		return WorkspaceContext{}, invalidContext()
	}
	return workspaceContextFromValue(decoded)
}

func workspaceContextFromValue(value any) (WorkspaceContext, error) {
	contextID, ok := field(value, "contextID").(string)
	if !ok || !IsCredentialKey(contextID) {
		return WorkspaceContext{}, invalidContext()
	}
	number, ok := generation(field(value, "generation"))
	if !ok {
		return WorkspaceContext{}, invalidContext()
	}
	return WorkspaceContext{contextID: contextID, generation: number}, nil
}

// workspaceContextFromReference reads the context of an invocation or
// command: exactly a reference.
func workspaceContextFromReference(value any) *WorkspaceContext {
	object, ok := value.(map[string]any)
	if !ok || len(object) != 2 {
		return nil
	}
	context, err := workspaceContextFromValue(object)
	if err != nil {
		return nil
	}
	return &context
}

// ID is the context's ID.
func (c WorkspaceContext) ID() string { return c.contextID }

// Generation is the context's generation.
func (c WorkspaceContext) Generation() uint64 { return c.generation }

func (c WorkspaceContext) toValue() map[string]any {
	return map[string]any{"contextID": c.contextID, "generation": c.generation}
}

// MarshalJSON writes { "contextID": ..., "generation": ... }.
func (c WorkspaceContext) MarshalJSON() ([]byte, error) { return encodeJSON(c.toValue()) }

// UnmarshalJSON reads a context reference or a full context.
func (c *WorkspaceContext) UnmarshalJSON(data []byte) error {
	value, err := decodeJSON(data)
	if err != nil {
		return invalidContext()
	}
	context, err := workspaceContextFromValue(value)
	if err != nil {
		return err
	}
	*c = context
	return nil
}

// Event is one lithe/host/subscriptions/notify.
type Event struct {
	subscriptionID string
	sequence       uint64
	gap            bool
	event          any
}

// SubscriptionID is the subscription the event belongs to.
func (e Event) SubscriptionID() string { return e.subscriptionID }

// Sequence starts at 1 and grows by one per event of the subscription.
func (e Event) Sequence() uint64 { return e.sequence }

// Gap is true when events before this one were lost, so state built from
// earlier events should be refreshed, for example by rescanning.
func (e Event) Gap() bool { return e.gap }

// Event is the event, usually an object.
func (e Event) Event() json.RawMessage { return raw(e.event) }

// Errors of [Subscription.RecvTimeout].
var (
	// ErrSubscriptionDisposed: the subscription was disposed or the session
	// ended.
	ErrSubscriptionDisposed = errors.New("The subscription was disposed.")
	// ErrRecvTimeout: no event arrived in time.
	ErrRecvTimeout = errors.New("No subscription event arrived in time.")
)

// eventQueue is an unbounded queue of one subscription's events, in order.
type eventQueue struct {
	mu     sync.Mutex
	items  []Event
	closed bool
	// signal is closed, and replaced, when the queue changes.
	signal chan struct{}
}

func newEventQueue() *eventQueue {
	return &eventQueue{signal: make(chan struct{})}
}

func (q *eventQueue) changed() {
	close(q.signal)
	q.signal = make(chan struct{})
}

func (q *eventQueue) push(event Event) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.items = append(q.items, event)
	q.changed()
}

func (q *eventQueue) close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	q.closed = true
	q.changed()
}

// pop waits for the next event: at most timeout when it is positive, else
// without a limit. It fails with ErrSubscriptionDisposed once the queue is
// closed and empty, and ErrRecvTimeout when the time ran out.
func (q *eventQueue) pop(timeout time.Duration) (Event, error) {
	var expired <-chan time.Time
	if timeout > 0 {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		expired = timer.C
	}
	for {
		q.mu.Lock()
		if len(q.items) > 0 {
			event := q.items[0]
			q.items[0] = Event{}
			q.items = q.items[1:]
			q.mu.Unlock()
			return event, nil
		}
		if q.closed {
			q.mu.Unlock()
			return Event{}, ErrSubscriptionDisposed
		}
		signal := q.signal
		q.mu.Unlock()
		select {
		case <-signal:
		case <-expired:
			return Event{}, ErrRecvTimeout
		}
	}
}

// Subscription is a host subscription, such as a file watch or registered
// editor actions. Events queue in order until read; dropping the
// subscription does not dispose it on the host, [Subscription.Dispose] does.
type Subscription struct {
	id     string
	shared *shared
	live   *atomic.Bool
	queue  *eventQueue
}

// ID is the subscription's ID.
func (s *Subscription) ID() string { return s.id }

// IsDisposed is true once disposed here or the session ended.
func (s *Subscription) IsDisposed() bool { return !s.live.Load() }

// Recv waits for the next event; false once disposed or the session ended.
func (s *Subscription) Recv() (Event, bool) {
	event, err := s.queue.pop(0)
	if err != nil || s.IsDisposed() {
		return Event{}, false
	}
	return event, true
}

// RecvTimeout waits at most timeout for the next event. It fails with
// ErrSubscriptionDisposed once disposed or the session ended, and with
// ErrRecvTimeout when no event arrived in time.
func (s *Subscription) RecvTimeout(timeout time.Duration) (Event, error) {
	if s.IsDisposed() {
		return Event{}, ErrSubscriptionDisposed
	}
	if timeout <= 0 {
		timeout = time.Nanosecond
	}
	event, err := s.queue.pop(timeout)
	if err != nil {
		return Event{}, err
	}
	if s.IsDisposed() {
		return Event{}, ErrSubscriptionDisposed
	}
	return event, nil
}

// Dispose stops routing events at once and asks the host to dispose the
// subscription. After the session ended there is nothing to ask.
func (s *Subscription) Dispose() error { return s.shared.dispose(s.id, s.live) }

// Credentials is the host's credentials service: secrets private to this
// plugin, kept in the platform's secret store, never in ${stateDir}. It
// needs the credentials.store permission; without it every request fails
// with plugin.host.permissionDenied.
type Credentials struct{ host *Host }

// Get returns the secret stored under key, with false when there is none.
func (c *Credentials) Get(key string) (string, bool, error) {
	if err := checkCredentialKey(key); err != nil {
		return "", false, err
	}
	result, err := c.host.request(methodCredentialsGet, map[string]any{"key": key})
	if err != nil {
		return "", false, err
	}
	switch value := result.(type) {
	case nil:
		return "", false, nil
	case map[string]any:
		if secret, ok := value["secret"].(string); ok {
			return secret, true, nil
		}
	}
	return "", false, newSdkError("sdk.host.invalidResult", "The host answered credentials/get without a secret.")
}

// Set stores secret (at most 16 KiB) under key, replacing any value.
func (c *Credentials) Set(key, secret string) error {
	if err := checkCredentialKey(key); err != nil {
		return err
	}
	if len(secret) > maxSecretBytes {
		return newSdkError("sdk.credentials.invalidSecret", "A secret is a string of at most 16 KiB.")
	}
	_, err := c.host.request(methodCredentialsSet, map[string]any{"key": key, "secret": secret})
	return err
}

// Delete removes the secret under key; it succeeds when nothing was stored.
func (c *Credentials) Delete(key string) error {
	if err := checkCredentialKey(key); err != nil {
		return err
	}
	_, err := c.host.request(methodCredentialsDel, map[string]any{"key": key})
	return err
}

func checkCredentialKey(key string) error {
	if IsCredentialKey(key) {
		return nil
	}
	return newSdkError("sdk.credentials.invalidKey", "A credential key is 1-128 ASCII letters, digits, \".\", \"_\", \":\" or \"-\".")
}

// Subscriptions disposes subscriptions by ID.
type Subscriptions struct{ host *Host }

// Dispose disposes a subscription by ID; it also succeeds for an unknown or
// disposed one.
func (s *Subscriptions) Dispose(subscriptionID string) error {
	return s.host.shared.dispose(subscriptionID, nil)
}

// Workspace is workspace contexts and files. Every method but ListContexts
// names a context: the call's [Context.WorkspaceContext], or one from
// ListContexts read with [ParseWorkspaceContext]. params is any
// JSON-serializable object, nil for none.
type Workspace struct{ host *Host }

// ListContexts answers { revision, contexts }.
func (w *Workspace) ListContexts() (json.RawMessage, error) {
	return w.host.objectOrNull(methodListContexts, map[string]any{})
}

// GetContext answers the context.
func (w *Workspace) GetContext(context WorkspaceContext) (json.RawMessage, error) {
	return w.host.scoped(methodGetContext, context, nil)
}

// GetFolders answers { folders }.
func (w *Workspace) GetFolders(context WorkspaceContext) (json.RawMessage, error) {
	return w.host.scoped(methodGetFolders, context, nil)
}

// HostPath is an absolute path on the remote host of a remote context, read
// with [Workspace.GetHostPaths]. It is not a path on this machine: give it
// only to a process that runs on that host, never to os, filepath or exec
// here. It is its own type so it is not passed as a local path by mistake.
type HostPath string

// String is the path as text.
func (p HostPath) String() string { return string(p) }

// HostFolder is a root of a remote context and its path on the remote host.
type HostFolder struct {
	RootID   string   `json:"rootID"`
	HostPath HostPath `json:"hostPath"`
}

// GetHostPaths answers the path on the remote host of each root of a remote
// context, or of rootIDs only: 1-256 distinct root IDs from GetFolders. It
// needs the workspace.metadata permission, and only a host that serves it
// lists it, so check Supports("lithe/host/workspace/getHostPaths") first. A
// local context, or a host that cannot answer, fails with
// plugin.host.authorityUnsupported.
func (w *Workspace) GetHostPaths(context WorkspaceContext, rootIDs ...string) ([]HostFolder, error) {
	params := map[string]any{"context": context.toValue()}
	if len(rootIDs) > 0 {
		if !distinctRootIDs(rootIDs) {
			return nil, newSdkError("sdk.host.invalidParams", "rootIDs is 1-256 distinct root IDs of 1-128 ASCII letters, digits, \".\", \"_\", \":\" or \"-\".")
		}
		ids := make([]any, len(rootIDs))
		for i, id := range rootIDs {
			ids[i] = id
		}
		params["rootIDs"] = ids
	}
	result, err := w.host.request(methodGetHostPaths, params)
	if err != nil {
		return nil, err
	}
	return hostFolders(result)
}

// ProcessRoot is a root of a context as a process started for it names its
// working directory: Path for a local context, HostPath for a remote one.
// The other one is empty.
type ProcessRoot struct {
	RootID   string   `json:"rootID"`
	Path     string   `json:"path,omitempty"`
	HostPath HostPath `json:"hostPath,omitempty"`
}

// ProcessRoots answers the roots of a full context from ListContexts or
// GetContext -- a json.RawMessage, a map, or any value that serializes to
// one. A local context's folders are read without asking the host; for any
// other authority it calls GetHostPaths.
func (w *Workspace) ProcessRoots(context any) ([]ProcessRoot, error) {
	value, err := toValue(context)
	kind, ok := pointer(value, "authority", "kind").(string)
	if err != nil || !ok {
		return nil, newSdkError("sdk.host.invalidContext", "ProcessRoots takes a full context, with its authority and folders, from ListContexts or GetContext.")
	}
	if kind == "local" {
		return localFolders(field(value, "folders"))
	}
	reference, err := workspaceContextFromValue(value)
	if err != nil {
		return nil, err
	}
	folders, err := w.GetHostPaths(reference)
	if err != nil {
		return nil, err
	}
	roots := make([]ProcessRoot, len(folders))
	for i, folder := range folders {
		roots[i] = ProcessRoot{RootID: folder.RootID, HostPath: folder.HostPath}
	}
	return roots, nil
}

func distinctRootIDs(rootIDs []string) bool {
	if len(rootIDs) > maxHostPathRoots {
		return false
	}
	seen := make(map[string]bool, len(rootIDs))
	for _, id := range rootIDs {
		if !IsCredentialKey(id) || seen[id] {
			return false
		}
		seen[id] = true
	}
	return true
}

// hostFolders reads { folders: [{ rootID, hostPath }] } of a GetHostPaths
// answer.
func hostFolders(result any) ([]HostFolder, error) {
	items, ok := field(result, "folders").([]any)
	if !ok {
		return nil, invalidHostPaths()
	}
	folders := make([]HostFolder, len(items))
	for i, item := range items {
		rootID, _ := field(item, "rootID").(string)
		path, _ := field(item, "hostPath").(string)
		if !IsCredentialKey(rootID) || !strings.HasPrefix(path, "/") {
			return nil, invalidHostPaths()
		}
		folders[i] = HostFolder{RootID: rootID, HostPath: HostPath(path)}
	}
	return folders, nil
}

func invalidHostPaths() *SdkError {
	return newSdkError("sdk.host.invalidResult", "The host answered getHostPaths without folders of a root ID and an absolute host path.")
}

// localFolders reads [{ rootID, path }] of a local context's folders.
func localFolders(value any) ([]ProcessRoot, error) {
	items, ok := value.([]any)
	if !ok {
		return nil, invalidLocalFolders()
	}
	roots := make([]ProcessRoot, len(items))
	for i, item := range items {
		rootID, _ := field(item, "rootID").(string)
		path, ok := field(item, "path").(string)
		if !IsCredentialKey(rootID) || !ok {
			return nil, invalidLocalFolders()
		}
		roots[i] = ProcessRoot{RootID: rootID, Path: path}
	}
	return roots, nil
}

func invalidLocalFolders() *SdkError {
	return newSdkError("sdk.host.invalidContext", "A local context's folders each carry a rootID and a path.")
}

// ReadDirectory takes resource, cursor?, limit?.
func (w *Workspace) ReadDirectory(context WorkspaceContext, params any) (json.RawMessage, error) {
	return w.host.scoped(methodReadDirectory, context, params)
}

// Stat takes resource.
func (w *Workspace) Stat(context WorkspaceContext, params any) (json.RawMessage, error) {
	return w.host.scoped(methodStat, context, params)
}

// ReadFile takes resource, maximumBytes?.
func (w *Workspace) ReadFile(context WorkspaceContext, params any) (json.RawMessage, error) {
	return w.host.scoped(methodReadFile, context, params)
}

// Watch watches resource, recursive?; events are { rootID, changedPaths,
// requiresRescan }.
func (w *Workspace) Watch(context WorkspaceContext, params any) (*Subscription, error) {
	value, err := toValue(params)
	if err != nil {
		return nil, invalidHostParams()
	}
	if value, err = withContext(context, value); err != nil {
		return nil, err
	}
	return w.host.subscribe(methodWatch, value)
}

// FindFiles takes a root resource, query, fileMask?, cursor?, limit?.
func (w *Workspace) FindFiles(context WorkspaceContext, params any) (json.RawMessage, error) {
	return w.host.scoped(methodFindFiles, context, params)
}

// FindText takes a root resource, pattern, and optional search options.
func (w *Workspace) FindText(context WorkspaceContext, params any) (json.RawMessage, error) {
	return w.host.scoped(methodFindText, context, params)
}

// Create takes resource, kind ("file", "directory" or "symlink"), text? for a
// file and linkTarget for a symlink, and answers { resource, kind }. A link
// stores linkTarget verbatim, absolute or relative to the link's directory,
// and makes nothing outside the root readable. It never overwrites: an
// existing resource is plugin.host.alreadyExists.
func (w *Workspace) Create(context WorkspaceContext, params any) (json.RawMessage, error) {
	return w.host.scoped(methodCreate, context, params)
}

// Rename takes resource and target, the full new path, and answers
// { resource }. The host saves the open documents under resource first, and
// they follow the file.
func (w *Workspace) Rename(context WorkspaceContext, params any) (json.RawMessage, error) {
	return w.host.scoped(methodRename, context, params)
}

// Copy takes resource and target, a path that does not exist, and answers
// { resource } of the copy.
func (w *Workspace) Copy(context WorkspaceContext, params any) (json.RawMessage, error) {
	return w.host.scoped(methodCopy, context, params)
}

// Trash takes resource and moves it to the system trash after saving and
// closing its open documents; nothing is deleted permanently.
func (w *Workspace) Trash(context WorkspaceContext, params any) (json.RawMessage, error) {
	return w.host.scoped(methodTrash, context, params)
}

// Documents is open documents, read from their live buffers.
type Documents struct{ host *Host }

// List answers { documents }.
func (d *Documents) List(context WorkspaceContext) (json.RawMessage, error) {
	return d.host.scoped(methodDocumentsList, context, nil)
}

// GetSnapshot takes documentID and answers { document, text }.
func (d *Documents) GetSnapshot(context WorkspaceContext, params any) (json.RawMessage, error) {
	return d.host.scoped(methodDocumentsSnap, context, params)
}

// Save takes documentID, expectedVersion? and answers { document } after the
// write. A file changed on disk is plugin.host.diskChanged, and nothing is
// written.
func (d *Documents) Save(context WorkspaceContext, params any) (json.RawMessage, error) {
	return d.host.scoped(methodDocumentsSave, context, params)
}

// SaveAs takes documentID and target, a path that does not exist, and answers
// { document } of the new file, which the host opens.
func (d *Documents) SaveAs(context WorkspaceContext, params any) (json.RawMessage, error) {
	return d.host.scoped(methodDocumentsAs, context, params)
}

// Close takes documentID, saves the document and then closes its tab; it
// never discards the user's edits.
func (d *Documents) Close(context WorkspaceContext, params any) (json.RawMessage, error) {
	return d.host.scoped(methodDocumentsClose, context, params)
}

// Editor is the text editor.
type Editor struct{ host *Host }

// GetActive answers the foreground text tab's document summary, or null.
func (e *Editor) GetActive(context WorkspaceContext) (json.RawMessage, error) {
	return e.host.scoped(methodEditorActive, context, nil)
}

// GetSelections takes documentID.
func (e *Editor) GetSelections(context WorkspaceContext, params any) (json.RawMessage, error) {
	return e.host.scoped(methodEditorSelect, context, params)
}

// ApplyEdits takes documentID, expectedVersion, edits.
func (e *Editor) ApplyEdits(context WorkspaceContext, params any) (json.RawMessage, error) {
	return e.host.scoped(methodEditorApply, context, params)
}

// ReplaceSelections takes documentID, expectedVersion, selections,
// replacements.
func (e *Editor) ReplaceSelections(context WorkspaceContext, params any) (json.RawMessage, error) {
	return e.host.scoped(methodEditorReplace, context, params)
}

// InsertText takes documentID, expectedVersion, at, text.
func (e *Editor) InsertText(context WorkspaceContext, params any) (json.RawMessage, error) {
	return e.host.scoped(methodEditorInsert, context, params)
}

// Open takes resource, activation?, range?, tab?.
func (e *Editor) Open(context WorkspaceContext, params any) (json.RawMessage, error) {
	return e.host.scoped(methodEditorOpen, context, params)
}

// Reveal takes resource, activation?, range?, tab?.
func (e *Editor) Reveal(context WorkspaceContext, params any) (json.RawMessage, error) {
	return e.host.scoped(methodEditorReveal, context, params)
}

// RegisterActions registers editor actions (id, function, selector) that
// invoke one of the backend's functions; disposing the subscription removes
// them.
func (e *Editor) RegisterActions(context WorkspaceContext, params any) (*Subscription, error) {
	value, err := toValue(params)
	if err != nil {
		return nil, invalidHostParams()
	}
	if value, err = withContext(context, value); err != nil {
		return nil, err
	}
	return e.host.subscribe(methodEditorActions, value)
}

// Preview is Markdown and HTML previews.
type Preview struct{ host *Host }

// OpenHTML takes an HTML resource or inline id/title/text, plus presentation?, range?.
func (p *Preview) OpenHTML(context WorkspaceContext, params any) (json.RawMessage, error) {
	return p.host.scoped(methodPreviewHTML, context, params)
}

// Browser is the system browser.
type Browser struct{ host *Host }

// OpenExternal takes exactly one of an HTTP(S) url or an HTML resource.
func (b *Browser) OpenExternal(context WorkspaceContext, params any) (json.RawMessage, error) {
	return b.host.scoped(methodBrowserOpen, context, params)
}

// Window is messages in the window. params: text, actions?; each method
// answers { selectedIndex }, null on dismissal.
type Window struct{ host *Host }

// ShowInformation shows an information message.
func (w *Window) ShowInformation(context WorkspaceContext, params any) (json.RawMessage, error) {
	return w.host.scoped(methodShowInfo, context, params)
}

// ShowWarning shows a warning message.
func (w *Window) ShowWarning(context WorkspaceContext, params any) (json.RawMessage, error) {
	return w.host.scoped(methodShowWarning, context, params)
}

// ShowError shows an error message.
func (w *Window) ShowError(context WorkspaceContext, params any) (json.RawMessage, error) {
	return w.host.scoped(methodShowError, context, params)
}

// Terminal is integrated native terminals owned by this backend session and
// workspace. Permission terminal.create; discover each method with
// [Host.Supports]. Input and output stay in the host terminal, not in the
// plugin's web page.
type Terminal struct{ host *Host }

// Create launches executable and arguments directly. params: executable,
// optional arguments, title, cwd resource, environment, and show (true).
func (t *Terminal) Create(context WorkspaceContext, params any) (json.RawMessage, error) {
	return t.host.scoped(methodTerminalCreate, context, params)
}

// Show shows an owned terminal in its original window without raising the
// window.
func (t *Terminal) Show(context WorkspaceContext, terminalID string) (json.RawMessage, error) {
	return t.host.scoped(methodTerminalShow, context, map[string]any{"terminalID": terminalID})
}

// Close stops the native process, keeping a bounded display suffix for
// reopen. One retainedContextBytes (0-262144) overrides the create-time
// choice.
func (t *Terminal) Close(context WorkspaceContext, terminalID string, retainedContextBytes ...uint64) (json.RawMessage, error) {
	params := map[string]any{"terminalID": terminalID}
	if len(retainedContextBytes) > 0 {
		params["retainedContextBytes"] = retainedContextBytes[0]
	}
	return t.host.scoped(methodTerminalClose, context, params)
}

// Reopen starts the saved argv again and restores plain display history,
// never input.
func (t *Terminal) Reopen(context WorkspaceContext, terminalID string) (json.RawMessage, error) {
	return t.host.scoped(methodTerminalReopen, context, map[string]any{"terminalID": terminalID})
}

// Dispose permanently forgets an owned handle and its history, also closing
// it if live.
func (t *Terminal) Dispose(context WorkspaceContext, terminalID string) (json.RawMessage, error) {
	return t.host.scoped(methodTerminalDrop, context, map[string]any{"terminalID": terminalID})
}

// OnExit subscribes to one exit event per process generation, including the
// latest recorded outcome when subscribing after exit. Disposing the
// subscription stops notifications, not the terminal itself.
func (t *Terminal) OnExit(context WorkspaceContext, terminalID string) (*Subscription, error) {
	params, err := withContext(context, map[string]any{"terminalID": terminalID})
	if err != nil {
		return nil, err
	}
	return t.host.subscribe(methodTerminalExit, params)
}

// Functions is calls to the functions other plugins export, relayed by
// Core. It needs a call the plugin declares as pluginFunctions.calls in its
// manifest; discover the methods with [Host.Supports]. The relay accepts
// local workspaces only.
type Functions struct{ host *Host }

// Providers answers { providers }: the enabled plugins other than this one
// that export functionID, sorted by plugin ID, each { pluginID, version }.
// This plugin must have declared the function.
func (f *Functions) Providers(functionID string) (json.RawMessage, error) {
	if !IsFunctionID(functionID) {
		return nil, invalidCall("functionID is a plugin function ID.")
	}
	return f.host.requestObject(methodProviders, map[string]any{"functionID": functionID})
}

// Invoke calls one provider and answers { value }, the provider's result.
// call is an object with pluginID and functionID, and optionally params (an
// object), timeoutMilliseconds (a positive integer; the host defaults it
// and caps it at 600000), and parentOperationID; any other key is not sent.
// context is a workspace context this backend learned from the workspace
// service.
//
// Inside a handler, parentOperationID defaults to the operationID of the
// call the handler serves, which tells Core the new call belongs to it. A
// [BackendHandle.Host] serves no call, so it sends the field only when call
// sets it. Cancelling the handler's call cancels the relayed call.
func (f *Functions) Invoke(context WorkspaceContext, call any) (json.RawMessage, error) {
	value, err := toValue(call)
	if err != nil {
		return nil, invalidCall("A cross-plugin call is an object.")
	}
	relayed, err := relayParams(value, f.host.parentOperation)
	if err != nil {
		return nil, err
	}
	params, err := withContext(context, relayed)
	if err != nil {
		return nil, err
	}
	return f.host.requestObject(methodRelayInvoke, params)
}

func invalidCall(message string) *SdkError {
	return newSdkError("sdk.functions.invalidCall", message)
}

// relayParams is the params of lithe/host/functions/invoke for call,
// without its context: the fields the method defines and no other, so the
// host never answers -32018. serving is the operationID parentOperationID
// defaults to, "" for none.
func relayParams(call any, serving string) (map[string]any, error) {
	object, ok := call.(map[string]any)
	if !ok {
		return nil, invalidCall("A cross-plugin call is an object.")
	}
	relayed := map[string]any{}
	pluginID, ok := object["pluginID"].(string)
	if !ok || pluginID == "" {
		return nil, invalidCall("pluginID is a non-empty string.")
	}
	relayed["pluginID"] = pluginID
	functionID, ok := object["functionID"].(string)
	if !ok || !IsFunctionID(functionID) {
		return nil, invalidCall("functionID is a plugin function ID.")
	}
	relayed["functionID"] = functionID
	if params, present := object["params"]; present {
		if _, ok := params.(map[string]any); !ok {
			return nil, invalidCall("params is an object.")
		}
		relayed["params"] = params
	}
	if timeout, present := object["timeoutMilliseconds"]; present {
		if milliseconds, ok := unsignedInteger(timeout); !ok || milliseconds == 0 {
			return nil, invalidCall("timeoutMilliseconds is a positive integer.")
		}
		relayed["timeoutMilliseconds"] = timeout
	}
	if parent, present := object["parentOperationID"]; present {
		text, ok := parent.(string)
		if !ok || !isParentOperationID(text) {
			return nil, invalidCall("parentOperationID is a 1-128 byte string.")
		}
		relayed["parentOperationID"] = text
	} else if serving != "" {
		relayed["parentOperationID"] = serving
	}
	return relayed, nil
}

// ShowQuickPick: Choose one or more stable item IDs. Permission window.input.
func (w *Window) ShowQuickPick(context WorkspaceContext, params any) (json.RawMessage, error) {
	return w.host.scoped(methodWindowShowQuickPick, context, params)
}

// ShowInputBox: Request text or a masked password. Permission window.input.
func (w *Window) ShowInputBox(context WorkspaceContext, params any) (json.RawMessage, error) {
	return w.host.scoped(methodWindowShowInputBox, context, params)
}

// OpenMarkdown: Show a resource or read-only inline Markdown. Permission preview.open.
func (w *Preview) OpenMarkdown(context WorkspaceContext, params any) (json.RawMessage, error) {
	return w.host.scoped(methodPreviewOpenMarkdown, context, params)
}

// ShowDiff: Compare two read-only inline texts. Permission editor.open.
func (w *Editor) ShowDiff(context WorkspaceContext, params any) (json.RawMessage, error) {
	return w.host.scoped(methodEditorShowDiff, context, params)
}

// Views provides context-scoped views services.
type Views struct{ host *Host }

// Views returns native views services.
func (h *Host) Views() *Views { return &Views{host: h} }

// Refresh: Invalidate a visible owned view instance. Permission views.refresh.
func (w *Views) Refresh(context WorkspaceContext, params any) (json.RawMessage, error) {
	return w.host.scoped(methodViewsRefresh, context, params)
}

// Output provides context-scoped output services.
type Output struct{ host *Host }

// Output returns native output services.
func (h *Host) Output() *Output { return &Output{host: h} }

// Create: Create an independent output channel. Permission output.write.
func (w *Output) Create(context WorkspaceContext, params any) (json.RawMessage, error) {
	return w.host.scoped(methodOutputCreate, context, params)
}

// Append: Append bounded output text. Permission output.write.
func (w *Output) Append(context WorkspaceContext, params any) (json.RawMessage, error) {
	return w.host.scoped(methodOutputAppend, context, params)
}

// Clear: Clear an output channel. Permission output.write.
func (w *Output) Clear(context WorkspaceContext, params any) (json.RawMessage, error) {
	return w.host.scoped(methodOutputClear, context, params)
}

// Show: Show an output channel. Permission output.write.
func (w *Output) Show(context WorkspaceContext, params any) (json.RawMessage, error) {
	return w.host.scoped(methodOutputShow, context, params)
}

// Dispose: Release an output channel. Permission output.write.
func (w *Output) Dispose(context WorkspaceContext, params any) (json.RawMessage, error) {
	return w.host.scoped(methodOutputDispose, context, params)
}

// Progress provides context-scoped progress services.
type Progress struct{ host *Host }

// Progress returns native progress services.
func (h *Host) Progress() *Progress { return &Progress{host: h} }

// Create: Create cancellable progress. Permission window.progress.
func (w *Progress) Create(context WorkspaceContext, params any) (*Subscription, error) {
	value, err := toValue(params)
	if err != nil {
		return nil, invalidHostParams()
	}
	value, err = withContext(context, value)
	if err != nil {
		return nil, err
	}
	return w.host.subscribe(methodProgressCreate, value)
}

// Report: Update progress text or percentage. Permission window.progress.
func (w *Progress) Report(context WorkspaceContext, params any) (json.RawMessage, error) {
	return w.host.scoped(methodProgressReport, context, params)
}

// End: Release progress. Permission window.progress.
func (w *Progress) End(context WorkspaceContext, params any) (json.RawMessage, error) {
	value, err := toValue(params)
	if err != nil {
		return nil, invalidHostParams()
	}
	result, err := w.host.scoped(methodProgressEnd, context, value)
	if err != nil {
		return nil, err
	}
	object, _ := value.(map[string]any)
	id, _ := object["progressID"].(string)
	if err := w.host.shared.dispose(id, nil); err != nil {
		return nil, err
	}
	return result, nil
}

// StatusBar provides context-scoped statusBar services.
type StatusBar struct{ host *Host }

// StatusBar returns native statusBar services.
func (h *Host) StatusBar() *StatusBar { return &StatusBar{host: h} }

// SetItem: Create or replace a status item. Permission statusBar.write.
func (w *StatusBar) SetItem(context WorkspaceContext, params any) (json.RawMessage, error) {
	return w.host.scoped(methodStatusBarSetItem, context, params)
}

// RemoveItem: Remove an owned status item. Permission statusBar.write.
func (w *StatusBar) RemoveItem(context WorkspaceContext, params any) (json.RawMessage, error) {
	return w.host.scoped(methodStatusBarRemoveItem, context, params)
}

// Clipboard provides context-scoped clipboard services.
type Clipboard struct{ host *Host }

// Clipboard returns native clipboard services.
func (h *Host) Clipboard() *Clipboard { return &Clipboard{host: h} }

// ReadText: Read system clipboard text. Permission clipboard.read.
func (w *Clipboard) ReadText(context WorkspaceContext, params any) (json.RawMessage, error) {
	return w.host.scoped(methodClipboardReadText, context, params)
}

// WriteText: Write system clipboard text. Permission clipboard.write.
func (w *Clipboard) WriteText(context WorkspaceContext, params any) (json.RawMessage, error) {
	return w.host.scoped(methodClipboardWriteText, context, params)
}

// Reveal: Reveal a saved resource in the file manager. Permission workspace.reveal.
func (w *Workspace) Reveal(context WorkspaceContext, params any) (json.RawMessage, error) {
	return w.host.scoped(methodWorkspaceReveal, context, params)
}

// Network provides context-scoped network services.
type Network struct{ host *Host }

// Network returns native network services.
func (h *Host) Network() *Network { return &Network{host: h} }

// Request: Send a bounded HTTP request with host proxy policy. Permission network.request.
func (w *Network) Request(context WorkspaceContext, params any) (json.RawMessage, error) {
	return w.host.scoped(methodNetworkRequest, context, params)
}
