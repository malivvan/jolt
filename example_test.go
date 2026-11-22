package js_test

import (
	"context"
	"fmt"

	canopy "github.com/malivvan/js"
)

func ExampleNew() {
	exec, err := canopy.New(canopy.Config{
		Name: "greet.js",
		Source: `
export function greet(name) {
  return utils.prefix + " " + name.trim();
}`,
		Globals: map[string]any{
			"utils": map[string]any{"prefix": "hello"},
		},
	})
	if err != nil {
		fmt.Println(err)
		return
	}
	defer exec.Close()

	out, err := exec.Call(context.Background(), "greet", "  world  ")
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(out)
	// Output: hello world
}

func ExampleFeatureFunc() {
	feature := canopy.FeatureFunc(func(env *canopy.Env) (func() error, error) {
		return nil, env.SetFunc("featureValue", func() string { return "from a feature" })
	})
	exec, err := canopy.New(canopy.Config{
		Name:     "feature.js",
		Source:   `export function value() { return featureValue(); }`,
		Features: []canopy.Feature{feature},
	})
	if err != nil {
		fmt.Println(err)
		return
	}
	defer exec.Close()

	out, err := exec.Call(context.Background(), "value")
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(out)
	// Output: from a feature
}

type exampleTask struct {
	Prompt string `json:"prompt"`
	N      int    `json:"n"`
}

func ExampleExecutor_CallInto() {
	exec, err := canopy.New(canopy.Config{
		Name: "build.js",
		Source: `
export function build(task) {
  return { prompt: task.prompt.trim(), n: (task.n ?? 1) * 2 };
}`,
	})
	if err != nil {
		fmt.Println(err)
		return
	}
	defer exec.Close()

	var out exampleTask
	if err := exec.CallInto(context.Background(), "build", &out, exampleTask{Prompt: " cat ", N: 3}); err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(out.Prompt, out.N)
	// Output: cat 6
}
