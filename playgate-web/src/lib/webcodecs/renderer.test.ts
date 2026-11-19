import { describe, expect, it, vi } from "vitest";
import { CanvasRenderer } from "./renderer";

function fakeFrame(w: number, h: number) {
  return {
    displayWidth: w,
    displayHeight: h,
    codedWidth: w,
    codedHeight: h,
    close: vi.fn(),
  } as unknown as VideoFrame;
}

describe("CanvasRenderer", () => {
  it("requests desynchronized 2d context and falls back to plain alpha:false", () => {
    const drawImage = vi.fn();
    const getContext = vi
      .fn()
      .mockReturnValueOnce(null)
      .mockReturnValueOnce({ drawImage });
    const canvas = { width: 0, height: 0, getContext } as unknown as HTMLCanvasElement;

    const r = new CanvasRenderer(canvas);
    expect(getContext).toHaveBeenNthCalledWith(1, "2d", { alpha: false, desynchronized: true });
    expect(getContext).toHaveBeenNthCalledWith(2, "2d", { alpha: false });

    r.start();
    const f = fakeFrame(1280, 720);
    r.submit(f);
    expect(drawImage).toHaveBeenCalledWith(f, 0, 0);
    expect(canvas.width).toBe(1280);
    expect(canvas.height).toBe(720);
    expect(r.drawn).toBe(1);
    expect(f.close).toHaveBeenCalledTimes(1);
  });

  it("draws synchronously on submit without requestAnimationFrame and closes frames when stopped", () => {
    const drawImage = vi.fn();
    const getContext = vi.fn().mockReturnValue({ drawImage });
    const canvas = { width: 300, height: 150, getContext } as unknown as HTMLCanvasElement;

    const r = new CanvasRenderer(canvas);
    // Before start(): frame is closed and not drawn.
    const f0 = fakeFrame(1280, 720);
    r.submit(f0);
    expect(drawImage).not.toHaveBeenCalled();
    expect(f0.close).toHaveBeenCalledTimes(1);
    expect(r.drawn).toBe(0);

    // After start(): drawn immediately on submit.
    r.start();
    const f1 = fakeFrame(1280, 720);
    r.submit(f1);
    expect(drawImage).toHaveBeenCalledTimes(1);
    expect(r.drawn).toBe(1);
    expect(f1.close).toHaveBeenCalledTimes(1);

    // After stop(): frame is closed and not drawn.
    r.stop();
    const f2 = fakeFrame(1280, 720);
    r.submit(f2);
    expect(drawImage).toHaveBeenCalledTimes(1);
    expect(r.drawn).toBe(1);
    expect(f2.close).toHaveBeenCalledTimes(1);
  });
});
