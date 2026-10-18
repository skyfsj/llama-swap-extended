// Package codec implements the conversions: one decoder and one encoder per
// wire dialect, whole-response conversion, and the streaming state machines
// that translate upstream frames into client frames.
//
// It is an implementation detail of the dialect package; the public surface
// (types, options and entry points) lives there.
package codec
