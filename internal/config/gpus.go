package config

import (
	"fmt"
	"sort"

	"gopkg.in/yaml.v3"
)

// GpusConfig is the "gpus" router configuration: a per-card declaration of
// which models may occupy each GPU card. Models listed on the same card are
// mutually exclusive — starting one evicts the others on that card — while
// models on disjoint cards may run concurrently. A model listed on several
// cards occupies all of them (a multi-GPU model); a model listed on no card
// occupies no card and may run alongside any other model.
type GpusConfig struct {
	// Cards maps a GPU card ID to the models that may occupy that card.
	Cards map[string][]string
}

// UnmarshalYAML decodes the card mapping. Card keys may be written as YAML
// strings or integers; both are normalized to the same string key.
func (c *GpusConfig) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.MappingNode {
		return fmt.Errorf("gpus must be a mapping of card to model list")
	}

	cards := make(map[string][]string, len(value.Content)/2)
	for i := 0; i < len(value.Content); i += 2 {
		keyNode := value.Content[i]
		valueNode := value.Content[i+1]

		if keyNode.Kind != yaml.ScalarNode {
			return fmt.Errorf("gpus card keys must be scalar values")
		}
		card := keyNode.Value
		if card == "" {
			return fmt.Errorf("gpus card key must not be empty")
		}
		if _, exists := cards[card]; exists {
			return fmt.Errorf("duplicate card %q in gpus", card)
		}

		var models []string
		if err := valueNode.Decode(&models); err != nil {
			return fmt.Errorf("gpus card %q: models must be a list of model IDs: %w", card, err)
		}
		cards[card] = models
	}

	c.Cards = cards
	return nil
}

// ValidateGpus checks the gpus configuration against the configured models.
func ValidateGpus(gpus *GpusConfig, models map[string]ModelConfig) error {
	if gpus == nil || len(gpus.Cards) == 0 {
		return fmt.Errorf("gpus must define at least one card")
	}

	for _, card := range sortedGpusCards(gpus.Cards) {
		seen := make(map[string]bool, len(gpus.Cards[card]))
		for _, model := range gpus.Cards[card] {
			if model == "" {
				return fmt.Errorf("gpus card %q contains an empty model ID", card)
			}
			if seen[model] {
				return fmt.Errorf("model %q is listed more than once on gpus card %q", model, card)
			}
			seen[model] = true
			if _, exists := models[model]; !exists {
				return fmt.Errorf("gpus card %q references unknown model %q", card, model)
			}
		}
	}
	return nil
}

// CardsOf returns the sorted list of cards that model is listed on.
func (c *GpusConfig) CardsOf(model string) []string {
	cards := make([]string, 0)
	for card, models := range c.Cards {
		for _, m := range models {
			if m == model {
				cards = append(cards, card)
				break
			}
		}
	}
	sort.Strings(cards)
	return cards
}

// SharesCard reports whether both models are listed on at least one common
// card. A model listed on no card never shares a card with anything.
func (c *GpusConfig) SharesCard(a, b string) bool {
	for _, card := range c.CardsOf(a) {
		for _, m := range c.Cards[card] {
			if m == b {
				return true
			}
		}
	}
	return false
}

func sortedGpusCards(cards map[string][]string) []string {
	out := make([]string, 0, len(cards))
	for card := range cards {
		out = append(out, card)
	}
	sort.Strings(out)
	return out
}
