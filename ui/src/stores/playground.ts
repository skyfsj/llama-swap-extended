import { persistentStore } from "./persistent";

export type PlaygroundTab = "chat" | "translation" | "images" | "concurrency";

export const playgroundTabs: { id: PlaygroundTab; labelKey: string }[] = [
  { id: "chat", labelKey: "playground.tabs.chat" },
  { id: "translation", labelKey: "playground.tabs.translation" },
  { id: "images", labelKey: "playground.tabs.images" },
  { id: "concurrency", labelKey: "playground.tabs.concurrency" },
];

// A previously persisted tab may belong to a removed module; fall back to chat
// so the page always renders with an active tab.
const storedTab = persistentStore<string>("playground-selected-tab", "chat");
const knownTabs = new Set<string>(playgroundTabs.map((t) => t.id));

export const selectedPlaygroundTab = {
  ...storedTab,
  subscribe: (run: (value: PlaygroundTab) => void) =>
    storedTab.subscribe((value) => run(knownTabs.has(value) ? (value as PlaygroundTab) : "chat")),
} as typeof storedTab;
