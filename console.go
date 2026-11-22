package js

import (
	"strings"

	"github.com/MoeForge/moejs"
	"github.com/MoeForge/moejs/engine"
)

// install adds the console global. The methods follow Node's console in
// spirit: log/info/debug write a line, warn/error also write a line (to the
// same writer), dir writes a JSON-ish representation, and assert writes an
// assertion failure.
func (c *consoleWriter) install(env *Env) error {
	line := func(_ *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
		c.write([]byte(formatConsoleArgs(env.Raw().Realm(), args) + "\n"))
		return moejs.Undefined(), nil
	}
	dir := func(r *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
		if len(args) == 0 {
			c.write([]byte("undefined\n"))
			return moejs.Undefined(), nil
		}
		c.write([]byte(formatConsoleValue(r, args[0]) + "\n"))
		return moejs.Undefined(), nil
	}
	assert := func(r *moejs.Realm, _ moejs.Value, args []moejs.Value) (moejs.Value, error) {
		if len(args) > 0 && engine.ToBoolean(args[0]) {
			return moejs.Undefined(), nil
		}
		rest := args
		if len(rest) > 0 {
			rest = rest[1:]
		}
		text := "Assertion failed"
		if len(rest) > 0 {
			text += ": " + formatConsoleArgs(r, rest)
		}
		c.write([]byte(text + "\n"))
		return moejs.Undefined(), nil
	}
	ns := map[string]any{
		"log":    moejs.NativeFunc(line),
		"info":   moejs.NativeFunc(line),
		"debug":  moejs.NativeFunc(line),
		"warn":   moejs.NativeFunc(line),
		"error":  moejs.NativeFunc(line),
		"trace":  moejs.NativeFunc(line),
		"dir":    moejs.NativeFunc(dir),
		"assert": moejs.NativeFunc(assert),
	}
	return env.SetNamespace("console", ns)
}

func formatConsoleArgs(r *moejs.Realm, args []moejs.Value) string {
	var b strings.Builder
	for i, arg := range args {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(formatConsoleValue(r, arg))
	}
	return b.String()
}

func formatConsoleValue(r *moejs.Realm, v moejs.Value) string {
	if v.IsString() {
		return v.String()
	}
	if v.IsObject() {
		if data, ok, err := r.AppendJSON(nil, v); err == nil && ok {
			return string(data)
		}
		return r.DisplayString(v)
	}
	return v.String()
}
