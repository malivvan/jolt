// Command greet runs a small JavaScript module from Go.
package main

import (
	"context"
	"fmt"
	"os"

	canopy "github.com/malivvan/js"
)

func main() {
	exec, err := canopy.New(canopy.Config{
		Name: "greet.js",
		Source: `
export function greet(name) {
  return utils.prefix + " " + name.trim() + "!";
}`,
		Globals: map[string]any{
			"utils": map[string]any{"prefix": "hello"},
		},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "compile:", err)
		os.Exit(1)
	}
	defer exec.Close()

	out, err := exec.Call(context.Background(), "greet", "  world  ")
	if err != nil {
		fmt.Fprintln(os.Stderr, "call:", err)
		os.Exit(1)
	}
	fmt.Println(out)
}
