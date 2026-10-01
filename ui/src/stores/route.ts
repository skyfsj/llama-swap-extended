import { writable } from "svelte/store";

function initialRouteFromLocation(): string {
  if (typeof window === "undefined") return "/";
  const hashRoute = window.location.hash.replace(/^#/, "");
  return hashRoute.startsWith("/") ? hashRoute : "/";
}

const initialRoute = initialRouteFromLocation();

export const currentRoute = writable(initialRoute);
