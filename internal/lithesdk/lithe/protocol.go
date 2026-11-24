package lithe

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Protocol versions this SDK speaks.
const (
	FunctionsVersion    = 0
	LoggingVersion      = 0
	KeymapImportVersion = 0
)

// JSON-RPC methods of the backend protocol.
const (
	methodInitialize          = "initialize"
	methodShutdown            = "shutdown"
	methodExit                = "exit"
	methodCancelRequest       = "$/cancelRequest"
	methodLog                 = "lithe/log"
	methodInvoke              = "lithe/function/invoke"
	methodRegister            = "lithe/function/register"
	methodUnregister          = "lithe/function/unregister"
	methodProgress            = "lithe/function/progress"
	methodKeymapImport        = "lithe/keymap/import"
	methodKeymapInputs        = "lithe/host/keymapImport/listInputs"
	methodKeymapChunk         = "lithe/host/keymapImport/readChunk"
	methodCredentialsGet      = "lithe/host/credentials/get"
	methodCredentialsSet      = "lithe/host/credentials/set"
	methodCredentialsDel      = "lithe/host/credentials/delete"
	methodCapabilities        = "lithe/host/getCapabilities"
	methodDispose             = "lithe/host/subscriptions/dispose"
	methodListContexts        = "lithe/host/workspace/listContexts"
	methodGetContext          = "lithe/host/workspace/getContext"
	methodGetFolders          = "lithe/host/workspace/getFolders"
	methodGetHostPaths        = "lithe/host/workspace/getHostPaths"
	methodReadDirectory       = "lithe/host/workspace/readDirectory"
	methodStat                = "lithe/host/workspace/stat"
	methodReadFile            = "lithe/host/workspace/readFile"
	methodWatch               = "lithe/host/workspace/watch"
	methodFindFiles           = "lithe/host/workspace/findFiles"
	methodFindText            = "lithe/host/workspace/findText"
	methodCreate              = "lithe/host/workspace/create"
	methodRename              = "lithe/host/workspace/rename"
	methodCopy                = "lithe/host/workspace/copy"
	methodTrash               = "lithe/host/workspace/trash"
	methodDocumentsList       = "lithe/host/documents/list"
	methodDocumentsSnap       = "lithe/host/documents/getSnapshot"
	methodDocumentsSave       = "lithe/host/documents/save"
	methodDocumentsAs         = "lithe/host/documents/saveAs"
	methodDocumentsClose      = "lithe/host/documents/close"
	methodEditorActions       = "lithe/host/editor/registerActions"
	methodEditorActive        = "lithe/host/editor/getActive"
	methodEditorSelect        = "lithe/host/editor/getSelections"
	methodEditorApply         = "lithe/host/editor/applyEdits"
	methodEditorReplace       = "lithe/host/editor/replaceSelections"
	methodEditorInsert        = "lithe/host/editor/insertText"
	methodEditorOpen          = "lithe/host/editor/open"
	methodEditorReveal        = "lithe/host/editor/reveal"
	methodWindowShowQuickPick = "lithe/host/window/showQuickPick"
	methodWindowShowInputBox  = "lithe/host/window/showInputBox"
	methodPreviewOpenMarkdown = "lithe/host/preview/openMarkdown"
	methodEditorShowDiff      = "lithe/host/editor/showDiff"
	methodViewsRefresh        = "lithe/host/views/refresh"
	methodOutputCreate        = "lithe/host/output/create"
	methodOutputAppend        = "lithe/host/output/append"
	methodOutputClear         = "lithe/host/output/clear"
	methodOutputShow          = "lithe/host/output/show"
	methodOutputDispose       = "lithe/host/output/dispose"
	methodProgressCreate      = "lithe/host/progress/create"
	methodProgressReport      = "lithe/host/progress/report"
	methodProgressEnd         = "lithe/host/progress/end"
	methodStatusBarSetItem    = "lithe/host/statusBar/setItem"
	methodStatusBarRemoveItem = "lithe/host/statusBar/removeItem"
	methodClipboardReadText   = "lithe/host/clipboard/readText"
	methodClipboardWriteText  = "lithe/host/clipboard/writeText"
	methodWorkspaceReveal     = "lithe/host/workspace/reveal"
	methodNetworkRequest      = "lithe/host/network/request"
	methodPreviewHTML         = "lithe/host/preview/openHTML"
	methodBrowserOpen         = "lithe/host/browser/openExternal"
	methodShowInfo            = "lithe/host/window/showInformation"
	methodShowWarning         = "lithe/host/window/showWarning"
	methodShowError           = "lithe/host/window/showError"
	methodTerminalCreate      = "lithe/host/terminal/create"
	methodTerminalShow        = "lithe/host/terminal/show"
	methodTerminalClose       = "lithe/host/terminal/close"
	methodTerminalReopen      = "lithe/host/terminal/reopen"
	methodTerminalDrop        = "lithe/host/terminal/dispose"
	methodTerminalExit        = "lithe/host/terminal/onExit"
	methodProviders           = "lithe/host/functions/providers"
	methodRelayInvoke         = "lithe/host/functions/invoke"
	methodContexts            = "lithe/host/workspace/didChangeContexts"
	methodNotify              = "lithe/host/subscriptions/notify"
	methodCommandRan          = "lithe/host/commands/didRun"
	hostPrefix                = "lithe/host/"
)

// JSON-RPC error numbers.
const (
	rpcParseError           = -32700
	rpcInvalidRequest       = -32600
	rpcMethodNotFound       = -32601
	rpcInvalidParams        = -32602
	rpcInternalError        = -32603
	rpcServerNotInitialized = -32002
	rpcRequestFailed        = -32000
	rpcRequestCancelled     = -32800
)

// Protocol limits.
const (
	MaxFunctions          = 256
	maxFunctionIDBytes    = 128
	maxPluginErrorCode    = 64
	maxErrorMessageBytes  = 1024
	maxResultBytes        = 1 << 20
	maxCredentialKeyBytes = 128
	maxSecretBytes        = 16 << 10
	maxPendingHostCalls   = 16
	maxEarlyEvents        = 128
	maxCommandIDBytes     = 128
	maxParentOperation    = 128
	maxHostPathRoots      = 256
	maxSafeInteger        = 1<<53 - 1
)

// IsFunctionID reports whether id is dot-separated lowerCamelCase segments
// of at most 128 bytes, the form of a plugin function ID.
func IsFunctionID(id string) bool {
	if id == "" || len(id) > maxFunctionIDBytes {
		return false
	}
	for _, segment := range strings.Split(id, ".") {
		if segment == "" || segment[0] < 'a' || segment[0] > 'z' {
			return false
		}
		for i := 1; i < len(segment); i++ {
			if !isASCIIAlnum(segment[i]) {
				return false
			}
		}
	}
	return true
}

// IsPluginErrorCode reports whether code can name a plugin error: a letter,
// then letters, digits, ".", "_" or "-", at most 64 bytes.
func IsPluginErrorCode(code string) bool {
	if code == "" || len(code) > maxPluginErrorCode || !isASCIILetter(code[0]) {
		return false
	}
	for i := 1; i < len(code); i++ {
		if !isASCIIAlnum(code[i]) && !strings.ContainsRune("._-", rune(code[i])) {
			return false
		}
	}
	return true
}

// IsCredentialKey reports whether key is 1-128 ASCII letters, digits, ".",
// "_", ":" or "-". Context and subscription IDs use the same form.
func IsCredentialKey(key string) bool {
	if key == "" || len(key) > maxCredentialKeyBytes {
		return false
	}
	for i := 0; i < len(key); i++ {
		if !isASCIIAlnum(key[i]) && !strings.ContainsRune("._:-", rune(key[i])) {
			return false
		}
	}
	return true
}

// IsCommandID reports whether id is 1-128 lowercase letters, digits, "." or
// "-", the form of a contributed command.
func IsCommandID(id string) bool {
	if id == "" || len(id) > maxCommandIDBytes {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !(c >= 'a' && c <= 'z') && !(c >= '0' && c <= '9') && c != '.' && c != '-' {
			return false
		}
	}
	return true
}

// isParentOperationID reports whether id can name the call a cross-plugin
// call belongs to: 1-128 bytes.
func isParentOperationID(id string) bool {
	return id != "" && len(id) <= maxParentOperation
}

func isASCIILetter(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

func isASCIIAlnum(c byte) bool { return isASCIILetter(c) || c >= '0' && c <= '9' }

// offers reports whether a capability offer lists protocol version exactly.
func offers(offer any, version int64) bool {
	object, ok := offer.(map[string]any)
	if !ok {
		return false
	}
	versions, ok := object["versions"].([]any)
	if !ok {
		return false
	}
	for _, value := range versions {
		if number, ok := value.(json.Number); ok {
			if parsed, err := strconv.ParseInt(string(number), 10, 64); err == nil && parsed == version {
				return true
			}
		}
	}
	return false
}

// generation reads a positive safe integer; 2 and 2.0 both count.
func generation(value any) (uint64, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	text := string(number)
	var parsed uint64
	if unsigned, err := strconv.ParseUint(text, 10, 64); err == nil {
		parsed = unsigned
	} else if strings.ContainsAny(text, ".eE") {
		float, err := strconv.ParseFloat(text, 64)
		if err != nil || float < 1 || float > maxSafeInteger || float != math.Trunc(float) {
			return 0, false
		}
		parsed = uint64(float)
	} else {
		return 0, false
	}
	if parsed < 1 || parsed > maxSafeInteger {
		return 0, false
	}
	return parsed, true
}

// unsignedInteger reads a non-negative JSON integer.
func unsignedInteger(value any) (uint64, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	parsed, err := strconv.ParseUint(string(number), 10, 64)
	return parsed, err == nil
}

// integer reads a JSON integer that fits in int64.
func integer(value any) (int64, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	parsed, err := strconv.ParseInt(string(number), 10, 64)
	return parsed, err == nil
}

var errTrailingData = errors.New("trailing data after the JSON value")

// boundedText cuts text to at most limit bytes on a character boundary.
func boundedText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	end := limit
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end]
}

// decodeJSON parses JSON keeping numbers exact.
func decodeJSON(data []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if decoder.More() {
		return nil, errTrailingData
	}
	return value, nil
}

// encodeJSON serializes value as compact JSON without HTML escaping, the
// bytes the host receives.
func encodeJSON(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), nil
}

// raw serializes a decoded value for a caller that unmarshals it.
func raw(value any) json.RawMessage {
	data, err := encodeJSON(value)
	if err != nil {
		return json.RawMessage("null")
	}
	return data
}

// toValue converts any JSON-serializable Go value into the SDK's dynamic
// form -- map[string]any, []any, string, json.Number, bool and nil -- by
// encoding and decoding it, so nested numbers are exact too.
func toValue(value any) (any, error) {
	if value == nil {
		return nil, nil
	}
	data, err := encodeJSON(value)
	if err != nil {
		return nil, err
	}
	return decodeJSON(data)
}

// field is value[key] when value is an object, else nil.
func field(value any, key string) any {
	object, _ := value.(map[string]any)
	return object[key]
}

// pointer follows keys through nested objects.
func pointer(value any, keys ...string) any {
	for _, key := range keys {
		value = field(value, key)
	}
	return value
}

// requestKey is a JSON-RPC ID as a map key; numbers and strings never
// collide.
func requestKey(id any) string {
	data, err := encodeJSON(id)
	if err != nil {
		return "null"
	}
	return string(data)
}
