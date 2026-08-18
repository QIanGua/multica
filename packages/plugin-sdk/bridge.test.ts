// @vitest-environment jsdom
import { beforeEach, describe, expect, it, vi } from "vitest";

// The guest half of the handshake. Sibling surfaces are mutually opaque but
// `parent.frames[i]` is an allowed cross-origin access, so another plugin on the
// same page can postMessage into this one. What stops it from becoming this
// surface's "host" is the two checks below — neither is visible from reading a
// plugin's own code, which is exactly why they are pinned here.

async function loadSdk() {
  vi.resetModules();
  return import("./index");
}

function initFrom(source: unknown, port: MessagePort) {
  const event = new MessageEvent("message", {
    data: { type: "multica:plugin-bridge-init", version: 1, theme: {} },
  });
  Object.defineProperty(event, "source", { value: source, configurable: true });
  Object.defineProperty(event, "ports", { value: [port], configurable: true });
  window.dispatchEvent(event);
}

const settle = () => new Promise((resolve) => setTimeout(resolve, 0));

describe("surface bridge handshake", () => {
  beforeEach(() => {
    vi.useRealTimers();
  });

  it("accepts a port from the embedder and answers on it", async () => {
    const { multica } = await loadSdk();
    const channel = new MessageChannel();
    const asked: unknown[] = [];
    channel.port2.onmessage = (event) => {
      asked.push(event.data);
      const request = event.data as { id: string };
      channel.port2.postMessage({ id: request.id, ok: true, status: 200, data: { workspace: {}, user: {}, config: {} } });
    };
    channel.port2.start();

    initFrom(window.parent, channel.port1);
    await multica.context.get(true);

    expect(asked).toHaveLength(1);
    expect(asked[0]).toMatchObject({ kind: "action", method: "GET", path: "/context" });
  });

  it("ignores an init from a window that is not the embedder", async () => {
    const { multica } = await loadSdk();
    const hostile = new MessageChannel();
    const seen: unknown[] = [];
    hostile.port2.onmessage = (event) => seen.push(event.data);
    hostile.port2.start();

    // A sibling plugin frame shouting an init with its own port.
    initFrom({} as Window, hostile.port1);
    void multica.context.get(true);
    await settle();

    expect(seen).toHaveLength(0);
  });

  it("keeps the first port when a second init arrives", async () => {
    const { multica } = await loadSdk();
    const real = new MessageChannel();
    const hijack = new MessageChannel();
    const onReal: unknown[] = [];
    const onHijack: unknown[] = [];
    real.port2.onmessage = (event) => onReal.push(event.data);
    hijack.port2.onmessage = (event) => onHijack.push(event.data);
    real.port2.start();
    hijack.port2.start();

    initFrom(window.parent, real.port1);
    // Even from the embedder's own window: a hijacker that lost the race must
    // not be able to take the channel over afterwards.
    initFrom(window.parent, hijack.port1);
    void multica.context.get(true);
    await settle();

    // The property under test is that the second init cannot capture the
    // channel — traffic keeps going to the port bound first, and the hijacker's
    // port stays silent.
    expect(onHijack).toHaveLength(0);
    expect(onReal.length).toBeGreaterThan(0);
  });
});
