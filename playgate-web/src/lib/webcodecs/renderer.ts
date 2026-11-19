/**
 * renderer.ts — low-latency canvas renderer (spec section 5.3).
 * Requests a desynchronized 2D context so the browser can bypass compositor
 * VSync queueing, draws each decoded VideoFrame immediately on submit() instead
 * of waiting for requestAnimationFrame, and always close()s every frame.
 */

export class CanvasRenderer {
  private ctx: CanvasRenderingContext2D | null;
  private running = false;
  drawn = 0;
  /** Frames replaced before they were drawn (always 0 in immediate mode). */
  superseded = 0;

  constructor(private readonly canvas: HTMLCanvasElement) {
    this.ctx =
      canvas.getContext("2d", { alpha: false, desynchronized: true }) ??
      canvas.getContext("2d", { alpha: false });
  }

  start(): void {
    this.running = true;
  }

  stop(): void {
    this.running = false;
  }

  submit(frame: VideoFrame): void {
    try {
      if (!this.running) return;
      const w = frame.displayWidth || frame.codedWidth;
      const h = frame.displayHeight || frame.codedHeight;
      if (this.canvas.width !== w || this.canvas.height !== h) {
        this.canvas.width = w;
        this.canvas.height = h;
      }
      this.ctx?.drawImage(frame, 0, 0);
      this.drawn++;
    } finally {
      frame.close();
    }
  }
}
