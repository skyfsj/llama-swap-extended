package config

import (
	"sort"

	matrixpkg "github.com/mostlygeek/llama-swap/internal/matrix"
	"gopkg.in/yaml.v3"
)

// PruneRemovedModels rewrites a patched configuration document so the
// references a removed model leaves behind cannot fail validation. Without it,
// deleting a model a routing block still names is rejected as a whole: the
// matrix, groups, selectors, gpus cards, profile pins and the scheduler
// priority map all validate their entries against the live model set, so one
// dangling name blocks the delete that would have removed it.
//
// The rewrite is deliberately conservative: it only drops references to models
// that no longer exist, never renames or invents anything, and never touches
// model files, aliases or any non-routing block. It returns the human-readable
// locations it adjusted, in document order, so the caller can tell the operator
// which blocks were changed on their behalf.
//
// Both spellings of a block are pruned when present: the canonical
// routing.router.settings.* tree and the legacy top-level groups:/matrix:
// keys the loader normalizes into it.
func PruneRemovedModels(doc *yaml.Node, removed []string) []string {
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 || len(removed) == 0 {
		return nil
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil
	}
	gone := make(map[string]bool, len(removed))
	for _, id := range removed {
		gone[id] = true
	}

	changed := make([]string, 0, 4)
	for _, location := range [][]string{
		{"routing", "router", "settings", "matrix"},
		{"matrix"},
	} {
		pruneMatrix(root, location, gone, &changed)
	}
	for _, location := range [][]string{
		{"routing", "router", "settings", "groups"},
		{"groups"},
	} {
		pruneGroupMembers(root, location, gone, &changed)
	}
	pruneSelectors(root, gone, &changed)
	pruneGpuCards(root, gone, &changed)
	pruneFifoPriority(root, gone, &changed)
	pruneProfilePins(root, gone, &changed)
	if len(changed) == 0 {
		return nil
	}
	return changed
}

// mapValues reads a mapping into key -> value nodes. A non-mapping node yields
// an empty map so every helper can treat an absent and a malformed block alike.
func mapValues(node *yaml.Node) map[string]*yaml.Node {
	values := make(map[string]*yaml.Node)
	if node == nil || node.Kind != yaml.MappingNode {
		return values
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i]
		if key.Kind == yaml.ScalarNode {
			values[key.Value] = node.Content[i+1]
		}
	}
	return values
}

// childNode walks a pointer chain of mapping keys, returning nil when any
// segment is absent or is not a mapping.
func childNode(root *yaml.Node, path ...string) *yaml.Node {
	current := root
	for _, key := range path {
		next, ok := mapValues(current)[key]
		if !ok {
			return nil
		}
		current = next
	}
	return current
}

// detach removes one key from its parent mapping, preserving the order of the
// untouched keys. An emptied parent mapping is removed by its own parent in a
// later step, so an empty block never survives to the loader.
func detach(parent *yaml.Node, key string) {
	if parent == nil || parent.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(parent.Content); i += 2 {
		if parent.Content[i].Value != key {
			continue
		}
		parent.Content = append(parent.Content[:i], parent.Content[i+2:]...)
		return
	}
}

// detachIfEmpty drops a mapping from its parent once pruning removed its last
// entry, keeping the document free of blocks the loader would reject as empty.
func detachIfEmpty(parent *yaml.Node, key string) {
	child, ok := mapValues(parent)[key]
	if !ok {
		return
	}
	if child.Kind == yaml.MappingNode && len(child.Content) == 0 {
		detach(parent, key)
		return
	}
	if child.Kind == yaml.SequenceNode && len(child.Content) == 0 {
		detach(parent, key)
	}
}

// pruneSeq drops the matching entries of a sequence in place.
func pruneSeq(node *yaml.Node, gone map[string]bool) []string {
	if node == nil || node.Kind != yaml.SequenceNode {
		return nil
	}
	var dropped []string
	kept := make([]*yaml.Node, 0, len(node.Content))
	for _, item := range node.Content {
		if item.Kind == yaml.ScalarNode && gone[item.Value] {
			dropped = append(dropped, item.Value)
			continue
		}
		kept = append(kept, item)
	}
	node.Content = kept
	return dropped
}

// pruneMap drops the entries of a mapping whose key (or whose value, when the
// value holds the model reference) names a removed model.
func pruneMap(node *yaml.Node, gone map[string]bool, valueIsReference bool) []string {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	var dropped []string
	for i := 0; i+1 < len(node.Content); {
		key, value := node.Content[i], node.Content[i+1]
		reference := key.Value
		if valueIsReference {
			reference = value.Value
		}
		if gone[reference] {
			dropped = append(dropped, key.Value)
			node.Content = append(node.Content[:i], node.Content[i+2:]...)
			continue
		}
		i += 2
	}
	return dropped
}

func locationLabel(location ...string) string {
	label := location[0]
	for _, segment := range location[1:] {
		label += "." + segment
	}
	return label
}

// pruneMatrix adjusts one matrix block: vars and evict_costs lose the entries
// naming a removed model, and every set expression loses the leaves and
// references that no longer resolve. Dropping a set empties the expressions
// that referenced it, so the pass repeats until it stops changing anything.
// A matrix left with no set is removed together with the router selection,
// which would otherwise be rejected for selecting a missing engine.
func pruneMatrix(root *yaml.Node, location []string, gone map[string]bool, changed *[]string) {
	node := childNode(root, location...)
	if node == nil {
		return
	}
	label := locationLabel(location...)
	if len(pruneMap(mapValues(node)["vars"], gone, true)) > 0 {
		*changed = append(*changed, label+".vars")
	}
	detachIfEmpty(node, "vars")
	// A var that lost its model no longer resolves, so an evict cost keyed by
	// it dangles too.
	vars := mapValues(node)["vars"]
	var droppedCosts []string
	for i := 0; i+1 < len(node.Content); {
		key, value := node.Content[i], node.Content[i+1]
		if key.Value != "evict_costs" || value.Kind != yaml.MappingNode {
			i += 2
			continue
		}
		for j := 0; j+1 < len(value.Content); {
			costKey := value.Content[j].Value
			if gone[costKey] || danglingVar(vars, costKey) {
				droppedCosts = append(droppedCosts, costKey)
				value.Content = append(value.Content[:j], value.Content[j+2:]...)
				continue
			}
			j += 2
		}
		break
	}
	if len(droppedCosts) > 0 {
		*changed = append(*changed, label+".evict_costs")
	}
	detachIfEmpty(node, "evict_costs")

	sets := mapValues(node)["sets"]
	if sets == nil || sets.Kind != yaml.MappingNode {
		return
	}
	surviving := make(map[string]bool, len(sets.Content)/2)
	for i := 0; i+1 < len(sets.Content); i += 2 {
		surviving[sets.Content[i].Value] = true
	}
	// An identifier survives when it still names a set in this matrix, or when
	// it resolves through the surviving vars to a model that still exists.
	keep := func(ident string) bool {
		if gone[ident] {
			return false
		}
		if surviving[ident] {
			return true
		}
		if target, ok := mapValues(vars)[ident]; ok {
			return !gone[target.Value]
		}
		return resolveKeepsModel(ident, root)
	}

	for {
		changedExpression := false
		for i := 0; i+1 < len(sets.Content); i += 2 {
			name, definition := sets.Content[i], sets.Content[i+1]
			filtered, ok := matrixpkg.FilterDefinition(matrixpkg.Definition{Name: name.Value, DSL: definition.Value}, keep)
			if !ok {
				sets.Content = append(sets.Content[:i], sets.Content[i+2:]...)
				delete(surviving, name.Value)
				// Restart the scan: another set may have referenced this one.
				changedExpression = true
				break
			}
			if filtered.DSL != definition.Value {
				definition.Value = filtered.DSL
				changedExpression = true
			}
		}
		if !changedExpression {
			break
		}
	}
	if len(sets.Content) > 0 {
		*changed = append(*changed, label+".sets")
		return
	}
	// Every set is gone: the matrix itself is no longer usable, so the router
	// falls back to the default group engine rather than failing the delete.
	detach(childNode(root, location[:len(location)-1]...), location[len(location)-1])
	resetRouterSelection(root)
	*changed = append(*changed, label)
}

// danglingVar reports whether a matrix var no longer resolves to a live model,
// either because it was just pruned or because its target is gone.
func danglingVar(vars *yaml.Node, ident string) bool {
	target, ok := mapValues(vars)[ident]
	return !ok || target.Value == ""
}

// resolveKeepsModel reports whether ident names a model that still exists.
// The document's models map is the authority.
func resolveKeepsModel(ident string, root *yaml.Node) bool {
	models := childNode(root, "models")
	for i := 0; i+1 < len(models.Content); i += 2 {
		if models.Content[i].Value == ident {
			return true
		}
	}
	return false
}

// resetRouterSelection clears a routing.router.use that selected the matrix,
// so the loader falls back to the default group engine.
func resetRouterSelection(root *yaml.Node) {
	routing := childNode(root, "routing")
	if routing == nil {
		return
	}
	router := mapValues(routing)["router"]
	if router == nil {
		return
	}
	detach(router, "use")
	settings := mapValues(router)["settings"]
	if settings == nil {
		return
	}
	detach(settings, "matrix")
}

// pruneGroupMembers drops removed models from group member lists. A group left
// with no member is removed: an empty group is routing dead weight, and the
// loader synthesizes a default group when none remain.
func pruneGroupMembers(root *yaml.Node, location []string, gone map[string]bool, changed *[]string) {
	groups := childNode(root, location...)
	if groups == nil || groups.Kind != yaml.MappingNode {
		return
	}
	label := locationLabel(location...)
	dropped := false
	for i := 0; i+1 < len(groups.Content); {
		group := groups.Content[i+1]
		if group.Kind != yaml.MappingNode {
			i += 2
			continue
		}
		if len(pruneSeq(mapValues(group)["members"], gone)) > 0 {
			dropped = true
		}
		members := mapValues(group)["members"]
		empty := members == nil || (members.Kind == yaml.SequenceNode && len(members.Content) == 0)
		if empty {
			// The group lost its last member: the rest of the block (swap, for
			// example) would keep it alive as an empty group.
			groups.Content = append(groups.Content[:i], groups.Content[i+2:]...)
			continue
		}
		i += 2
	}
	if dropped {
		*changed = append(*changed, label+".members")
	}
	if len(groups.Content) == 0 {
		detach(childNode(root, location[:len(location)-1]...), location[len(location)-1])
	}
}

// pruneSelectors drops the targets that named a removed model. A selector left
// with no target is removed: the loader requires at least one entry, and a
// selector that resolves to nothing would fail every request routed through it.
func pruneSelectors(root *yaml.Node, gone map[string]bool, changed *[]string) {
	selectors := childNode(root, "selectors")
	if selectors == nil || selectors.Kind != yaml.MappingNode {
		return
	}
	dropped := false
	for i := 0; i+1 < len(selectors.Content); i += 2 {
		selector := selectors.Content[i+1]
		if selector.Kind != yaml.MappingNode {
			continue
		}
		if len(pruneSeq(mapValues(selector)["targets"], gone)) > 0 {
			dropped = true
		}
		targets := mapValues(selector)["targets"]
		if targets != nil && targets.Kind == yaml.SequenceNode && len(targets.Content) == 0 {
			selectors.Content = append(selectors.Content[:i], selectors.Content[i+2:]...)
			i -= 2
			continue
		}
	}
	if dropped {
		*changed = append(*changed, "selectors.targets")
	}
	if len(selectors.Content) == 0 {
		detach(root, "selectors")
	}
}

// pruneGpuCards drops removed models from every card list. A card left empty is
// removed: the loader requires at least one card with a model on it.
func pruneGpuCards(root *yaml.Node, gone map[string]bool, changed *[]string) {
	gpus := childNode(root, "routing", "router", "settings", "gpus")
	if gpus == nil || gpus.Kind != yaml.MappingNode {
		return
	}
	dropped := false
	for i := 0; i+1 < len(gpus.Content); i += 2 {
		card := gpus.Content[i+1]
		if len(pruneSeq(card, gone)) > 0 {
			dropped = true
		}
		if card.Kind == yaml.SequenceNode && len(card.Content) == 0 {
			gpus.Content = append(gpus.Content[:i], gpus.Content[i+2:]...)
			i -= 2
			continue
		}
	}
	if !dropped {
		return
	}
	*changed = append(*changed, "routing.router.settings.gpus")
	if len(gpus.Content) == 0 {
		settings := childNode(root, "routing", "router", "settings")
		detach(settings, "gpus")
		router := childNode(root, "routing", "router")
		detach(router, "use")
	}
}

// pruneFifoPriority drops the priority entries of removed models. The loader
// rejects a priority map naming an unknown model.
func pruneFifoPriority(root *yaml.Node, gone map[string]bool, changed *[]string) {
	fifo := childNode(root, "routing", "scheduler", "settings", "fifo")
	if fifo == nil {
		return
	}
	if len(pruneMap(mapValues(fifo)["priority"], gone, false)) == 0 {
		return
	}
	*changed = append(*changed, "routing.scheduler.settings.fifo.priority")
	detachIfEmpty(fifo, "priority")
}

// pruneProfilePins drops the pins that rewrite to a removed model. A profile
// left with no pin is removed: the loader requires at least one entry.
func pruneProfilePins(root *yaml.Node, gone map[string]bool, changed *[]string) {
	profiles := childNode(root, "profiles")
	if profiles == nil || profiles.Kind != yaml.MappingNode {
		return
	}
	dropped := false
	for i := 0; i+1 < len(profiles.Content); i += 2 {
		profile := profiles.Content[i+1]
		if profile.Kind != yaml.MappingNode {
			continue
		}
		if len(pruneMap(mapValues(profile)["pins"], gone, true)) > 0 {
			dropped = true
		}
		pins := mapValues(profile)["pins"]
		if pins != nil && pins.Kind == yaml.MappingNode && len(pins.Content) == 0 {
			detach(profile, "pins")
		}
		if profile.Kind == yaml.MappingNode && len(profile.Content) == 0 {
			profiles.Content = append(profiles.Content[:i], profiles.Content[i+2:]...)
			i -= 2
			continue
		}
	}
	if dropped {
		*changed = append(*changed, "profiles.pins")
	}
	if len(profiles.Content) == 0 {
		detach(root, "profiles")
	}
}

// ModelKeys returns the sorted top-level model IDs of a configuration
// document.
func ModelKeys(doc *yaml.Node) []string {
	models := childNode(documentRoot(doc), "models")
	if models == nil || models.Kind != yaml.MappingNode {
		return nil
	}
	keys := make([]string, 0, len(models.Content)/2)
	for i := 0; i+1 < len(models.Content); i += 2 {
		if models.Content[i].Kind == yaml.ScalarNode {
			keys = append(keys, models.Content[i].Value)
		}
	}
	sort.Strings(keys)
	return keys
}

// RemovedModelKeys returns the model IDs present in before and absent in the
// document, so a caller can tell which references a patch left dangling.
func RemovedModelKeys(before []string, after *yaml.Node) []string {
	present := make(map[string]bool)
	for _, key := range ModelKeys(after) {
		present[key] = true
	}
	var removed []string
	for _, key := range before {
		if !present[key] {
			removed = append(removed, key)
		}
	}
	sort.Strings(removed)
	return removed
}
