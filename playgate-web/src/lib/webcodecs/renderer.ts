/**
 * renderer.ts — canvas renderer (spec section 5.3).
 * Keeps at most one undrawn VideoFrame (latest wins), draws it in
 * requestAnimationFrame, and always close()s every frame.
 */

export class CanvasRenderer {
  private ctx: CanvasRenderingContext2D | null;
  private pending: VideoFrame | null = null;
  private raf = 0;
  private running = false;
  drawn = 0;
  /** Frames replaced before they were drawn. */
  superseded = 0;

  constructor(private readonly canvas: HTMLCanvasElement) {
    this.ctx = canvas.getContext("2d", { alpha: false });
  }

  start(): void {
    if (this.running) return;
    this.running = true;
    this.raf = requestAnimationFrame(this.loop);
  }

  stop(): void {
    this.running = false;
    cancelAnimationFrame(this.raf);
    this.pending?.close();
    this.pending = null;
  }

  submit(frame: VideoFrame): void {
    if (!this.running) {
      frame.close();
      return;
    }
    if (this.pending) {
      this.pending.close();
      this.superseded++;
    }
    this.pending = frame;
  }

  private loop = (): void => {
    if (!this.running) return;
    this.raf = requestAnimationFrame(this.loop);
    const f = this.pending;
    if (!f) return;
    this.pending = null;
    try {
      const w = f.displayWidth || f.codedWidth;
      const h = f.displayHeight || f.codedHeight;
      if (this.canvas.width !== w || this.canvas.height !== h) {
        this.canvas.width = w;
        this.canvas.height = h;
      }
      this.ctx?.drawImage(f, 0, 0);
      this.drawn++;
    } finally {
      f.close();
    }
  };
}
