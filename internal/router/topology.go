package router

import (
	"fmt"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/router/scheduler"
)

// plannerForConfig compiles the candidate topology without touching any
// process. The base router installs the returned planner atomically in its
// run-loop and keeps compatible process generations alive.
func plannerForConfig(conf config.Config, loggers ...*logmon.Monitor) (scheduler.Swapper, map[string]struct{}, error) {
	use := conf.Routing.Router.Use
	if use == "" {
		use = "group"
	}
	switch use {
	case "group":
		modelToGroup, err := groupModelToGroup(conf)
		if err != nil {
			return nil, nil, err
		}
		ids := make(map[string]struct{}, len(modelToGroup))
		for id := range modelToGroup {
			ids[id] = struct{}{}
		}
		return newGroupSwapper(conf, modelToGroup), ids, nil
	case "matrix":
		matrix := conf.Routing.Router.Settings.Matrix
		if matrix == nil {
			return nil, nil, fmt.Errorf("matrix router requires a matrix configuration")
		}
		if matrix.Program() == nil {
			if err := config.ValidateMatrix(matrix, conf.Models); err != nil {
				return nil, nil, fmt.Errorf("compiling matrix configuration: %w", err)
			}
		}
		var logger *logmon.Monitor
		if len(loggers) > 0 {
			logger = loggers[0]
		}
		if logger == nil {
			logger = logmon.NewWriter(nil)
		}
		ids := make(map[string]struct{}, len(conf.Models))
		for id := range conf.Models {
			ids[id] = struct{}{}
		}
		return &matrixSwapper{solver: newMatrixSolver(matrix.Program(), matrix.ResolvedEvictCosts()), logger: logger}, ids, nil
	case "gpus":
		gpus := conf.Routing.Router.Settings.Gpus
		if gpus == nil {
			return nil, nil, fmt.Errorf("gpus router requires a gpus configuration")
		}
		if err := config.ValidateGpus(gpus, conf.Models); err != nil {
			return nil, nil, fmt.Errorf("validating gpus configuration: %w", err)
		}
		var logger *logmon.Monitor
		if len(loggers) > 0 {
			logger = loggers[0]
		}
		if logger == nil {
			logger = logmon.NewWriter(nil)
		}
		ids := make(map[string]struct{}, len(conf.Models))
		for id := range conf.Models {
			ids[id] = struct{}{}
		}
		return &gpusSwapper{gpus: gpus, logger: logger}, ids, nil
	default:
		return nil, nil, fmt.Errorf("unsupported router type: %q", use)
	}
}
