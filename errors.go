package jolt

import (
	"context"
	"errors"
	"fmt"

	"github.com/MoeForge/moejs"
)

// Errors returned by jolt itself. Errors produced by the engine keep their
// type; for example a JavaScript throw is still an *Exception, and errors.As
// finds it through jolt's *Error wrapper.
var (
	// ErrClosed is returned when a Runtime or Executor is used after Close.
	ErrClosed = errors.New("jolt: closed")
	// ErrNotFunction is returned when a path or value names something that is
	// not callable.
	ErrNotFunction = errors.New("jolt: value is not a function")
	// ErrModuleNotFound is wrapped by resolution errors; a custom
	// ModuleLoader or resolver may return it to fall through to the next
	// loader.
	ErrModuleNotFound = errors.New("jolt: module not found")
	// ErrNotFound is returned when an explicit lookup (Export) does not find
	// the requested name. Get returns an undefined Value instead.
	ErrNotFound = errors.New("jolt: not found")
	// ErrNoSource is returned by New when Config names neither Source nor
	// SourceFile.
	ErrNoSource = errors.New("jolt: no source: set Config.Source or Config.SourceFile")
	// ErrReentrant is returned when a Runtime method that drives the event
	// loop is called from JavaScript running on that same loop, where a
	// nested loop cannot run.
	ErrReentrant = errors.New("jolt: cannot drive the event loop from inside JavaScript")
)

// Aliases for the engine's error and exception types, so users of the
// high-level API do not have to import moejs for the common cases.
type (
	// Exception is a thrown JavaScript value.
	Exception = moejs.Exception
	// InterruptedError is returned when an interrupt stopped JavaScript.
	InterruptedError = moejs.InterruptedError
	// MemoryLimitError is the value of the InterruptedError a runtime stops
	// with when it passes Config.MemoryLimit.
	MemoryLimitError = moejs.MemoryLimitError
	// SyntaxError is a parse or early error in source.
	SyntaxError = moejs.SyntaxError
	// InternalError is a Go panic that escaped the engine.
	InternalError = moejs.InternalError
)

// ErrTooLarge is what errors.Is finds in the error of a decode that passed a
// Config.Decode limit.
var ErrTooLarge = moejs.ErrTooLarge

// Error wraps an engine or host error with the operation and path jolt was
// running. It implements Unwrap, so errors.Is and errors.As reach the cause.
type Error struct {
	Op   string // "compile", "load", "call", "eval", "get", "set", ...
	Path string // function or value path, when there is one
	Err  error
}

func (e *Error) Error() string {
	switch {
	case e.Path != "":
		return "jolt: " + e.Op + " " + e.Path + ": " + e.Err.Error()
	default:
		return "jolt: " + e.Op + ": " + e.Err.Error()
	}
}

func (e *Error) Unwrap() error { return e.Err }

// JSError is a JavaScript error value converted to Go, such as the rejection
// reason of a promise. Name, Message and Stack are read from the value when
// it is an Error object.
type JSError struct {
	Name    string
	Message string
	Stack   string
	Value   any // the reason converted by ToGo
}

func (e *JSError) Error() string {
	name := e.Name
	if name == "" {
		name = "Error"
	}
	msg := e.Message
	if msg == "" {
		msg = fmt.Sprint(e.Value)
	}
	return name + ": " + msg
}

// PanicError is a Go panic recovered from a registered Go function. It is
// thrown to JavaScript as an Error. Stack is the Go stack at the panic.
type PanicError struct {
	Func  string
	Value any
	Stack []byte
}

func (e *PanicError) Error() string {
	if e.Func != "" {
		return fmt.Sprintf("jolt: panic in %s: %v", e.Func, e.Value)
	}
	return fmt.Sprintf("jolt: panic: %v", e.Value)
}

func wrap(op, path string, err error) error {
	if err == nil {
		return nil
	}
	return &Error{Op: op, Path: path, Err: err}
}

// IsException reports whether err is, or wraps, a JavaScript throw.
func IsException(err error) bool {
	var exc *Exception
	return errors.As(err, &exc)
}

// AsException returns the JavaScript exception in err, if any.
func AsException(err error) (*Exception, bool) {
	var exc *Exception
	if errors.As(err, &exc) {
		return exc, true
	}
	return nil, false
}

// IsInterrupted reports whether err is, or wraps, an engine interrupt. A
// timeout or a cancelled context is reported this way.
func IsInterrupted(err error) bool {
	var intr *InterruptedError
	return errors.As(err, &intr)
}

// IsMemoryLimit reports whether err is, or wraps, a Config.MemoryLimit hit.
func IsMemoryLimit(err error) bool {
	return errors.Is(err, moejs.ErrMemoryLimit)
}

// IsTimeout reports whether err is, or wraps, a context deadline.
func IsTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var intr *InterruptedError
	if errors.As(err, &intr) {
		return errors.Is(intr.Unwrap(), context.DeadlineExceeded)
	}
	return false
}

// IsCanceled reports whether err is, or wraps, a cancelled context.
func IsCanceled(err error) bool {
	if errors.Is(err, context.Canceled) {
		return true
	}
	var intr *InterruptedError
	if errors.As(err, &intr) {
		return errors.Is(intr.Unwrap(), context.Canceled)
	}
	return false
}

// JSStack returns the JavaScript stack trace carried by err, or "".
func JSStack(err error) string {
	var exc *Exception
	if errors.As(err, &exc) {
		return exc.Stack
	}
	var js *JSError
	if errors.As(err, &js) {
		return js.Stack
	}
	var pe *PanicError
	if errors.As(err, &pe) {
		return string(pe.Stack)
	}
	return ""
}

// isBroken reports whether a runtime must not be reused after err. An
// interrupt can leave timers, holds and promises pending; an internal error
// leaves the engine in an unknown state. A JavaScript exception is catchable
// and leaves the runtime reusable.
func isBroken(err error) bool {
	if err == nil {
		return false
	}
	if IsInterrupted(err) || IsMemoryLimit(err) {
		return true
	}
	var ie *InternalError
	return errors.As(err, &ie)
}
