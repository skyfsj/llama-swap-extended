package server

import (
	"fmt"

	"github.com/mostlygeek/llama-swap/pkg/dialect"
)

func validateExtensionToolChoice(state *extensionRequestState, name string) error {
	if state.unknownToolChoice {
		return fmt.Errorf("the client tool-selection policy cannot authorize server execution of %s", name)
	}
	choice := state.toolChoice
	if choice == nil {
		return nil
	}
	if choice.Mode == dialect.ToolChoiceNone {
		return fmt.Errorf("tool_choice:none forbids server execution of %s", name)
	}
	if choice.Mode == dialect.ToolChoiceTool && choice.Name != name {
		return fmt.Errorf("the client selected %s, not server tool %s", choice.Name, name)
	}
	if choice.AllowedNames != nil {
		for _, allowed := range choice.AllowedNames {
			if allowed == name {
				return nil
			}
		}
		return fmt.Errorf("server tool %s is outside the client's allowed tool set", name)
	}
	return nil
}
