package ui

// The pinned gotk4-webkitgtk bindings generate EvaluateJavascriptFinish but not
// the async start half, so the one call the reader needs — fire-and-forget
// script evaluation into the shell page — is done directly against the C API.
// The view pointer travels as guintptr and is cast in C, keeping Go free of a
// uintptr→unsafe.Pointer conversion (which go vet flags).

// #cgo pkg-config: webkitgtk-6.0
// #include <stdlib.h>
// #include <webkit/webkit.h>
//
// static void mb_eval_js(guintptr view, const char *script, gssize length) {
// 	webkit_web_view_evaluate_javascript((WebKitWebView *)view, script, length,
// 		NULL, NULL, NULL, NULL, NULL);
// }
import "C"

import (
	"unsafe"

	webkit "github.com/diamondburned/gotk4-webkitgtk/pkg/webkit/v6"
	coreglib "github.com/diamondburned/gotk4/pkg/core/glib"
)

// evalJS evaluates script in the web view's page, fire-and-forget (no result,
// no completion callback). Main thread only, like every other WebView call.
func evalJS(v *webkit.WebView, script string) {
	cs := C.CString(script)
	defer C.free(unsafe.Pointer(cs))
	C.mb_eval_js(C.guintptr(coreglib.BaseObject(v).Native()), cs, C.gssize(len(script)))
}
