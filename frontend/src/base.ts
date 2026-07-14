// The Go server resolves the external path prefix akari is served under (a
// reverse proxy may mount it anywhere, e.g. /proxy/akari) and injects it into
// the entry document as window.__AKARI_BASE_PATH__ (see
// internal/server/frontend/embed.go). A Vite dev serve has no injector and
// runs at the origin root.
declare global {
  interface Window {
    __AKARI_BASE_PATH__?: string;
  }
}

export const basePath = window.__AKARI_BASE_PATH__ ?? "";

// withBase externalizes a rooted path. The router handles its own paths via
// basename, but everything that leaves the router (fetch, EventSource, raw
// anchors, window.location) resolves against the external URL space and must
// pass through here.
export function withBase(path: string): string {
  return basePath + path;
}
