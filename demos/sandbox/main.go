// Command sandbox runs a JavaScript module with no file system, no network,
// a memory limit and one host function.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/malivvan/js"
)

const source = `
export function run(input) {
  if (typeof fetch !== "undefined") {
    throw new Error("fetch should not be installed");
  }
  if (input.a < 0 || input.b < 0) {
    throw new RangeError("negative inputs are not supported");
  }
  return host.sum(input.a, input.b);
}`

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	exec, err := js.New(js.Config{
		Name:        "sandbox.js",
		Source:      source,
		FS:          js.NoFS{},
		Network:     js.NoNetwork{},
		MemoryLimit: 8 << 20,
		Globals: map[string]any{
			"host": map[string]any{
				"sum": func(a, b int) int { return a + b },
			},
		},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "compile:", err)
		os.Exit(1)
	}
	defer exec.Close()

	var out int
	if err := exec.CallInto(ctx, "run", &out, map[string]any{"a": 20, "b": 22}); err != nil {
		fmt.Fprintln(os.Stderr, "call:", err)
		os.Exit(1)
	}
	fmt.Println(out)
}
