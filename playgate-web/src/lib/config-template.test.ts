import { describe, it, expect } from "vitest";
import { buildHostConfig } from "./config-template";

const OPTS = {
  roomId: "room-abc123",
  signalingUrl: "https://signaling.example.workers.dev",
  serverUrl: "https://server.example.com",
  apiKey: "hk_test+key/secret==",
  publicKeyBase64: "MCowBQYDK2VdAyEA+abc/def0123==",
};

const yaml = buildHostConfig(OPTS);

describe("buildHostConfig", () => {
  it("contains all required section keys", () => {
    for (const section of ["capture:", "encoder:", "abr:", "webrtc:", "input:", "session:", "signaling:", "metrics:", "server:"]) {
      expect(yaml).toContain(section);
    }
  });

  it("embeds the room id", () => {
    expect(yaml).toContain(OPTS.roomId);
  });

  it("embeds the signaling url", () => {
    expect(yaml).toContain(OPTS.signalingUrl);
  });

  it("embeds the server url", () => {
    expect(yaml).toContain(OPTS.serverUrl);
  });

  it("double-quotes the api key", () => {
    expect(yaml).toContain(`"${OPTS.apiKey}"`);
  });

  it("double-quotes the public key", () => {
    expect(yaml).toContain(`"${OPTS.publicKeyBase64}"`);
  });

  it("enables session (session.enabled: true)", () => {
    expect(yaml).toContain("enabled: true");
  });

  it("sets input target to nxbt", () => {
    expect(yaml).toContain("target: nxbt");
  });

  it("leaves no template placeholders", () => {
    expect(yaml).not.toMatch(/<[^>]+>/);
    expect(yaml).not.toContain("{{");
  });

  it("ends with a newline", () => {
    expect(yaml.endsWith("\n")).toBe(true);
  });
});
