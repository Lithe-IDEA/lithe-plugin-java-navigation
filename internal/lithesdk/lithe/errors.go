package lithe

import (
	"errors"
	"fmt"
	"strings"
)

// SDK error codes a caller may compare against.
const (
	CodeCallCancelled = "sdk.call.cancelled"
	CodeSessionClosed = "sdk.session.closed"
)

// ErrorCode pairs a host or relay error code with its JSON-RPC number.
type ErrorCode struct {
	Code   string
	Number int64
}

// HostErrorCodes are the codes Lithe answers host requests with, and the
// number each one travels as.
var HostErrorCodes = []ErrorCode{
	{"plugin.host.unavailable", -32601},
	{"plugin.host.invalidParams", -32602},
	{"plugin.host.failed", -32603},
	{"plugin.host.cancelled", -32800},
	{"plugin.host.permissionDenied", -32010},
	{"plugin.host.contextExpired", -32011},
	{"plugin.host.authorityUnsupported", -32012},
	{"plugin.host.versionConflict", -32013},
	{"plugin.host.limitExceeded", -32014},
	{"plugin.host.timeout", -32015},
	{"plugin.host.notFound", -32016},
	{"plugin.host.readOnly", -32017},
	{"plugin.host.unsupportedOption", -32018},
	{"plugin.host.preconditionFailed", -32019},
	{"plugin.host.busy", -32020},
	{"plugin.host.rateLimited", -32021},
	{"plugin.host.languageServerUnavailable", -32022},
	{"plugin.host.languageServerError", -32023},
	{"plugin.host.methodNotDeclared", -32024},
	{"plugin.host.toolUnresolved", -32025},
	{"plugin.host.foreignContribution", -32026},
	{"plugin.host.remoteDisconnected", -32078},
	{"plugin.host.platformUnavailable", -32079},
	{"plugin.host.alreadyExists", -32080},
	{"plugin.host.diskChanged", -32081},
}

// RelayErrorCodes are the codes a cross-plugin call fails with.
var RelayErrorCodes = []ErrorCode{
	{"plugin.function.notDeclared", -32027},
	{"plugin.function.notExported", -32028},
	{"plugin.function.providerDisabled", -32029},
	{"plugin.function.versionMismatch", -32030},
	{"plugin.function.callCycle", -32065},
	{"plugin.function.depthExceeded", -32066},
	{"plugin.function.tooManyCalls", -32067},
	{"plugin.function.payloadTooLarge", -32068},
	{"plugin.function.providerUnavailable", -32069},
	{"plugin.function.failed", -32070},
	{"plugin.function.invalidResult", -32071},
}

// PluginError is a failure the plugin reports to the caller with its own
// code; the caller sees the code and the message.
type PluginError struct {
	code    string
	message string
}

// NewPluginError returns a plugin error. It panics when code is not a
// plugin error code: a code is part of the plugin's API, so a malformed one
// is a programming error.
func NewPluginError(code, message string) *PluginError {
	if !IsPluginErrorCode(code) {
		panic(fmt.Sprintf("Plugin error code %q must start with a letter, use letters, digits, \".\", \"_\" or \"-\", and be at most 64 bytes.", code))
	}
	return &PluginError{code: code, message: message}
}

// Code is the plugin's error code.
func (e *PluginError) Code() string { return e.code }

// Message is the text the caller sees.
func (e *PluginError) Message() string { return e.message }

func (e *PluginError) Error() string { return e.code + ": " + e.message }

// ErrCancelled ends a call that stopped because it was cancelled. Return it
// (or wrap it) from a handler that saw its cancellation.
var ErrCancelled = errors.New("The call was cancelled.")

// SdkError is a failure of the SDK or of a host request: a host refusal
// keeps the code, JSON-RPC number and details Lithe sent.
type SdkError struct {
	code    string
	message string
	rpcCode *int64
	details map[string]any
}

func newSdkError(code, message string) *SdkError {
	return &SdkError{code: code, message: message}
}

// Code is the error code, such as "plugin.host.permissionDenied".
func (e *SdkError) Code() string { return e.code }

// Message is the text of the error.
func (e *SdkError) Message() string { return e.message }

// RPCCode is the JSON-RPC error number of a host refusal.
func (e *SdkError) RPCCode() (int64, bool) {
	if e.rpcCode == nil {
		return 0, false
	}
	return *e.rpcCode, true
}

// Details are the structured details of a host refusal, or nil.
func (e *SdkError) Details() map[string]any { return e.details }

func (e *SdkError) Error() string { return e.code + ": " + e.message }

// Is makes errors.Is(err, ErrCancelled) true for a cancelled host request.
func (e *SdkError) Is(target error) bool {
	return target == ErrCancelled && e.code == CodeCallCancelled
}

func unavailable(message string) *SdkError {
	number := int64(rpcMethodNotFound)
	return &SdkError{code: "plugin.host.unavailable", message: message, rpcCode: &number}
}

func cancelledError() *SdkError {
	return newSdkError(CodeCallCancelled, "The request was cancelled.")
}

func closedError() *SdkError {
	return newSdkError(CodeSessionClosed, "The session ended.")
}

// sdkErrorFromResponse maps a JSON-RPC error a host request was answered with.
func sdkErrorFromResponse(method string, value any) *SdkError {
	object, _ := value.(map[string]any)
	var number *int64
	if raw, ok := object["code"]; ok {
		if parsed, ok := integer(raw); ok {
			number = &parsed
		}
	}
	data, _ := object["data"].(map[string]any)
	code, ok := data["code"].(string)
	if !ok {
		if strings.HasPrefix(method, hostPrefix) {
			code = "plugin.host.failed"
			if number != nil {
				for _, table := range [][]ErrorCode{HostErrorCodes, RelayErrorCodes} {
					for _, known := range table {
						if known.Number == *number {
							code = known.Code
						}
					}
				}
			}
		} else {
			code = "sdk.host.error"
		}
	}
	message, ok := object["message"].(string)
	if !ok {
		message = method + " failed."
	}
	details, _ := data["details"].(map[string]any)
	return &SdkError{code: code, message: message, rpcCode: number, details: details}
}

// errorKind sorts a handler error into the three answers a call can give.
type errorKind int

const (
	kindPlugin errorKind = iota
	kindCancelled
	kindInternal
)

func classify(err error) (errorKind, *PluginError) {
	var plugin *PluginError
	if errors.As(err, &plugin) {
		return kindPlugin, plugin
	}
	if errors.Is(err, ErrCancelled) {
		return kindCancelled, nil
	}
	return kindInternal, nil
}

// failureKind is the fixed category a failure is logged with; its text can
// hold user data and is never logged.
func failureKind(err error) string {
	switch kind, _ := classify(err); kind {
	case kindPlugin:
		return "plugin_error"
	case kindCancelled:
		return "cancelled"
	default:
		return "internal_error"
	}
}

func cancelledResponse() map[string]any {
	return map[string]any{"code": rpcRequestCancelled, "message": "The request was cancelled."}
}

func internalResponse() map[string]any {
	return map[string]any{"code": rpcInternalError, "message": "The plugin backend failed unexpectedly."}
}

// responseError is the JSON-RPC error a failed call is answered with. An
// internal error's text stays in the backend.
func responseError(err error) map[string]any {
	switch kind, plugin := classify(err); kind {
	case kindCancelled:
		return cancelledResponse()
	case kindPlugin:
		return map[string]any{
			"code":    rpcRequestFailed,
			"message": boundedText(plugin.message, maxErrorMessageBytes),
			"data":    map[string]any{"code": plugin.code},
		}
	default:
		return internalResponse()
	}
}
