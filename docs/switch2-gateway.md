# Switch 2 gateway input

`input.target: gateway` drives the [PlayGate Switch 2 gateway](https://github.com/virgil724/playgate-switch2-gateway): an ESP32-C6 that presents itself to a Nintendo Switch 2 as a Pro Controller 2 over BLE. The host streams controller state to it over USB serial. The firmware handles pairing and all Bluetooth work.

Compared with `nxbt`:

- No Python daemon, BlueZ or Bluetooth adapter is needed on the host.
- Works on Windows, Linux and macOS.
- The device-side latency is fixed: the gateway reports at 100 Hz on a 5 ms BLE connection interval. Its README measures 5–7 ms from receiving a frame to submitting the HID report.

The trade-off is that it only works with a **Switch 2**. For a Switch 1, keep using `nxbt`.

## Setup

1. Flash the gateway firmware (see its README; ESP-IDF v6.1.0).
2. Pair it with the console once. The firmware keeps the pairing in NVS and reconnects on its own after reboot or console sleep.
3. Plug the gateway's **native USB** port (USB Serial/JTAG) into the host machine.
4. Configure the host:

   ```yaml
   input:
     target: gateway
     gateway:
       port: auto        # or COM5 / /dev/ttyACM0
       rate_hz: 90
       keepalive_ms: 40
   ```

`port: auto` looks for the one Espressif USB Serial/JTAG port (VID `303A`, PID `1001`). Set the port explicitly in these cases:

- More than one ESP32 is connected.
- You are on macOS with a `CGO_ENABLED=0` build, where enumeration is unavailable.

### Linux permissions

The port is normally `/dev/ttyACM0` and is owned by the `dialout` group:

```bash
sudo usermod -aG dialout "$USER"   # log out and back in
```

With Docker, pass the device through instead of mounting the nxbt socket. You do not need `--privileged` or the DBus mount for gateway input.

```bash
docker run ... --device /dev/ttyACM0 ...
```

## How the host talks to it

Each controller state becomes a 17-byte input payload: buttons, four stick axes and a sequence number. It is wrapped as `[1][0x01][17][payload][crc16]`, COBS-encoded and terminated with `0x00`. The PlayGate button bits and stick sign (negative Y is up) already match the gateway's, so no remapping is done.

**Keepalive.** The gateway neutralises the controller after 100 ms without input. So while anything is held, the host re-sends the latest state every `keepalive_ms` (40 ms). A neutral state is re-sent once per second.

**Sequence numbers.** Every frame has a new sequence number, because the gateway drops frames that are not newer than the last one it accepted. The counter starts from the wall clock, so a restarted host is not mistaken for stale input.

**Edge spacing.** Button edges are spaced `1/rate_hz` apart by the shared coalescer (`internal/input/coalesce`). At 90 Hz a quick press and release land in separate 100 Hz reports instead of merging into one. Stick-only updates coalesce to the latest position.

**Reconnects.** When the port fails (cable pulled, gateway reset), the host reopens it with backoff and re-sends the held state.

## Monitoring

The gateway pushes a 20-byte status report about once a second and whenever something changes:

- `hid` in the `gateway status` log line shows the console link. The host's input target reports `connected` once this is `connected`.
- A `gateway rejected input` warning means one of the gateway's error counters grew:
  - `invalid`: malformed frames;
  - `stale`: an old sequence number;
  - `conflicts`: another input source, for example the gateway's web page, held the lease.
- The gateway's own receive-to-submit time feeds the `daemon` stage of the host latency metrics, and host-side queueing feeds the `input` stage.
