package lithe

import (
	"encoding/json"
	"maps"
	"strconv"
)

// Plugin views: the event a view function receives and the result it
// returns. A tool window or settings region names a view function in
// view.function (contract v1, "Plugin views"). Declare it with
// [Backend.View]: the SDK reads the params into a [ViewEvent] and answers
// the handler's [ViewResult]. View and canvas nodes are JSON values -- maps,
// structs, or json.RawMessage -- shaped as plugin-ui.schema.json describes;
// Core checks every result before the host draws it.

const codeInvalidViewEvent = "sdk.view.invalidEvent"

// ViewEventKind is why the host called the view function.
type ViewEventKind string

const (
	// ViewOpen: the view became visible; answer a whole view.
	ViewOpen ViewEventKind = "open"
	// ViewClose: the view went away; the result is ignored.
	ViewClose    ViewEventKind = "close"
	ViewClick    ViewEventKind = "click"
	ViewChange   ViewEventKind = "change"
	ViewSelect   ViewEventKind = "select"
	ViewActivate ViewEventKind = "activate"
	ViewViewport ViewEventKind = "viewport"
	// ViewResize: the view's width stopped changing, for a view with a
	// canvas whose on lists resize; ViewEvent.Width is the new width.
	ViewResize ViewEventKind = "resize"
	// ViewSettings: the effective or draft setting values changed.
	ViewSettings ViewEventKind = "settings"
	ViewRefresh  ViewEventKind = "refresh"
	ViewExpand   ViewEventKind = "expand"
	ViewLoadMore ViewEventKind = "loadMore"
)

func parseViewEventKind(name string) (ViewEventKind, bool) {
	switch kind := ViewEventKind(name); kind {
	case ViewOpen, ViewClose, ViewClick, ViewChange, ViewSelect, ViewActivate, ViewViewport, ViewResize, ViewSettings, ViewRefresh, ViewExpand, ViewLoadMore:
		return kind, true
	}
	return "", false
}

// SurfaceKind is the kind of declaration being drawn.
type SurfaceKind string

const (
	SurfaceToolWindow    SurfaceKind = "toolWindow"
	SurfaceSettingRegion SurfaceKind = "settingRegion"
)

// ViewSurface is the declaration being drawn: a tool window or a settings
// region, by its ID.
type ViewSurface struct {
	Kind SurfaceKind
	ID   string
}

// Viewport is the visible part of a canvas, in view-box coordinates.
type Viewport struct {
	X, Y, Width, Height float64
}

// ViewEvent is the params of one call to a view function. An optional
// string field is "" when the host did not send it.
type ViewEvent struct {
	Kind ViewEventKind
	// Instance is host-chosen; each ViewOpen starts a new instance.
	Instance string
	Surface  ViewSurface
	// Language is the interface language tag, such as en or zh-Hans;
	// return view text in it.
	Language string
	// Effective holds the saved setting values in force, by setting ID.
	Effective map[string]any
	// Draft holds values changed in the open settings window but not
	// applied; empty in a tool window.
	Draft map[string]any
	// Node is the node acted on: the group inside a canvas, the tree for an
	// item.
	Node string
	// Canvas is the canvas ID of a click inside a canvas and of a viewport
	// event.
	Canvas string
	// Value is a change's value: a string, a bool for a toggle, or the bound
	// setting's new draft value (a json.Number for a number); nil when
	// absent.
	Value any
	// Setting is the setting ID of a change of a control bound to one.
	Setting string
	// Item is the tree item ID of a select or activate event.
	Item string
	// Cursor is the opaque continuation of a tree loadMore event.
	Cursor   string
	Viewport *Viewport
	// Width is the width in points the host lays the view out in, on every
	// event from a host that reports it. List resize in a canvas's on only
	// when it is present: a host without it may refuse the canvas.
	Width *float64
}

// ParseViewEvent reads the params the host sent; it fails with
// sdk.view.invalidEvent when a required field is missing or a field has the
// wrong type. An optional field that is null counts as absent.
func ParseViewEvent(params json.RawMessage) (*ViewEvent, error) {
	value, err := decodeJSON(params)
	if err != nil {
		return nil, invalidViewEvent()
	}
	return parseViewEvent(value)
}

func invalidViewEvent() *SdkError {
	return newSdkError(codeInvalidViewEvent, "Invalid view event.")
}

// optionalString reads fields[key]: absent or null is "", a string is
// itself, and anything else is invalid.
func optionalString(fields map[string]any, key string) (string, bool, error) {
	switch value := fields[key].(type) {
	case nil:
		return "", false, nil
	case string:
		return value, true, nil
	default:
		return "", false, invalidViewEvent()
	}
}

func number(value any) (float64, bool) {
	text, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	parsed, err := strconv.ParseFloat(string(text), 64)
	return parsed, err == nil
}

func parseViewEvent(params any) (*ViewEvent, error) {
	object, ok := params.(map[string]any)
	if !ok {
		return nil, invalidViewEvent()
	}
	name, _ := object["event"].(string)
	kind, ok := parseViewEventKind(name)
	if !ok {
		return nil, invalidViewEvent()
	}
	surfaceFields, ok := object["surface"].(map[string]any)
	if !ok {
		return nil, invalidViewEvent()
	}
	toolWindow, isToolWindow, err := optionalString(surfaceFields, "toolWindowID")
	if err != nil {
		return nil, err
	}
	region, isRegion, err := optionalString(surfaceFields, "settingRegionID")
	if err != nil {
		return nil, err
	}
	var surface ViewSurface
	switch {
	case isToolWindow && !isRegion:
		surface = ViewSurface{SurfaceToolWindow, toolWindow}
	case isRegion && !isToolWindow:
		surface = ViewSurface{SurfaceSettingRegion, region}
	default:
		return nil, invalidViewEvent()
	}
	settings, ok := object["settings"].(map[string]any)
	if !ok {
		return nil, invalidViewEvent()
	}
	effective, ok := settings["effective"].(map[string]any)
	if !ok {
		return nil, invalidViewEvent()
	}
	draft, ok := settings["draft"].(map[string]any)
	if !ok {
		return nil, invalidViewEvent()
	}
	event := &ViewEvent{Kind: kind, Surface: surface, Effective: maps.Clone(effective), Draft: maps.Clone(draft)}
	if event.Instance, ok = object["instance"].(string); !ok {
		return nil, invalidViewEvent()
	}
	if event.Language, ok = object["language"].(string); !ok {
		return nil, invalidViewEvent()
	}
	switch viewport := object["viewport"].(type) {
	case nil:
	case map[string]any:
		var fields [4]float64
		for i, key := range []string{"x", "y", "width", "height"} {
			if fields[i], ok = number(viewport[key]); !ok {
				return nil, invalidViewEvent()
			}
		}
		event.Viewport = &Viewport{fields[0], fields[1], fields[2], fields[3]}
	default:
		return nil, invalidViewEvent()
	}
	if width, present := object["width"]; present && width != nil {
		parsed, ok := number(width)
		if !ok {
			return nil, invalidViewEvent()
		}
		event.Width = &parsed
	}
	switch value := object["value"].(type) {
	case nil:
	case string, bool, json.Number:
		event.Value = value
	default:
		return nil, invalidViewEvent()
	}
	for key, target := range map[string]*string{"node": &event.Node, "canvas": &event.Canvas, "setting": &event.Setting, "item": &event.Item, "cursor": &event.Cursor} {
		if *target, _, err = optionalString(object, key); err != nil {
			return nil, err
		}
	}
	return event, nil
}

// ViewResult is what a view function answers. The first result after
// ViewOpen is a whole view. The zero value is [Unchanged].
type ViewResult struct {
	kind    resultKind
	nodes   []any
	patches []Patch
}

type resultKind int

const (
	resultUnchanged resultKind = iota
	resultView
	resultPatches
)

// View answers the whole view: its nodes, top to bottom.
func View(nodes ...any) ViewResult {
	return ViewResult{kind: resultView, nodes: nodes}
}

// Patches answers edits to the view the host is drawing, applied in order.
func Patches(patches ...Patch) ViewResult {
	return ViewResult{kind: resultPatches, patches: patches}
}

// Unchanged answers that nothing changed.
func Unchanged() ViewResult { return ViewResult{} }

// MarshalJSON writes {"view": [...]}, {"patches": [...]}, or {}.
func (r ViewResult) MarshalJSON() ([]byte, error) {
	switch r.kind {
	case resultView:
		nodes := r.nodes
		if nodes == nil {
			nodes = []any{}
		}
		return encodeJSON(map[string]any{"view": nodes})
	case resultPatches:
		patches := r.patches
		if patches == nil {
			patches = []Patch{}
		}
		return encodeJSON(map[string]any{"patches": patches})
	default:
		return []byte("{}"), nil
	}
}

// Patch is one edit, addressed by node ID within the host controls, or
// within one canvas scene ([Patch.InCanvas]) or one tree's items
// ([Patch.InTree]). Each method returns a new patch.
type Patch struct {
	fields map[string]any
}

func newPatch(op, id string, hasID bool) Patch {
	fields := map[string]any{"op": op}
	if hasID {
		fields["id"] = id
	}
	return Patch{fields}
}

func (p Patch) with(key string, value any) Patch {
	fields := maps.Clone(p.fields)
	if fields == nil {
		fields = map[string]any{}
	}
	fields[key] = value
	return Patch{fields}
}

// PatchSet merges fields, a JSON object, into the node; a null field
// removes it.
func PatchSet(id string, fields any) Patch {
	return newPatch("set", id, true).with("fields", fields)
}

// PatchReplace swaps the node, with its subtree, for node.
func PatchReplace(id string, node any) Patch {
	return newPatch("replace", id, true).with("node", node)
}

// PatchRemove deletes the node and its subtree.
func PatchRemove(id string) Patch {
	return newPatch("remove", id, true)
}

// PatchInsert adds node at the end of the top level; see [Patch.Under] and
// [Patch.At].
func PatchInsert(node any) Patch {
	return newPatch("insert", "", false).with("node", node)
}

// Under makes an insert go into the children of parent.
func (p Patch) Under(parent string) Patch { return p.with("parent", parent) }

// At makes an insert go at index instead of the end.
func (p Patch) At(index int) Patch { return p.with("index", index) }

// InCanvas addresses the nodes of canvas canvas.
func (p Patch) InCanvas(canvas string) Patch { return p.with("canvas", canvas) }

// InTree addresses the items of tree tree.
func (p Patch) InTree(tree string) Patch { return p.with("tree", tree) }

// MarshalJSON writes the patch object.
func (p Patch) MarshalJSON() ([]byte, error) {
	if p.fields == nil {
		return []byte("{}"), nil
	}
	return encodeJSON(p.fields)
}
