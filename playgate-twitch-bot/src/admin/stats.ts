/**
 * stats.ts — in-memory counters for the admin dashboard.
 *
 * Written by the pipeline, read by the admin server. Process-lifetime only;
 * the durable record of grants lives in the GrantStore.
 */
export interface StatsSnapshot {
  whispered: number;
  fallback: number;
  chat: number;
  failed: number;
  denied: number;
  errors: number;
  lastError: string | null;
}

export class Stats {
  private s: StatsSnapshot = {
    whispered: 0,
    fallback: 0,
    chat: 0,
    failed: 0,
    denied: 0,
    errors: 0,
    lastError: null,
  };

  delivered(via: "whisper" | "fallback" | "chat"): void {
    if (via === "whisper") this.s.whispered++;
    else if (via === "fallback") this.s.fallback++;
    else this.s.chat++;
  }

  deliveryFailed(): void {
    this.s.failed++;
  }

  denied(): void {
    this.s.denied++;
  }

  error(message: string): void {
    this.s.errors++;
    this.s.lastError = message;
  }

  snapshot(): StatsSnapshot {
    return { ...this.s };
  }
}
