//go:build windows

package main

import "fmt"

func main() {
	fmt.Println("vllm-wrapper is not supported on windows: it manages a linux vLLM serve process via unix signals")
}
