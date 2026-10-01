import { writable } from "svelte/store";

// Shared flag so the app shell (which owns the primary sidebar) can warn the
// user before in-app navigation away from Settings when there are unsaved
// changes. Settings keeps this in sync with its dirty state.
export const settingsDirty = writable(false);
