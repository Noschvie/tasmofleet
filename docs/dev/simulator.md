# The Tasmota simulator

Part of the working guide: start at [CLAUDE.md](../../CLAUDE.md).


`internal/simulator` replays real captures from `testdata/fixtures/devices/` and models
the OTA state machine documented in `docs/research/tasmota-minimal-and-ota.md`
(pending, downloading, reboot gap, minimal firmware with its 5 commands, settings not
persisted in minimal). Use it for every test that would otherwise need a device:

```go
fleet, _ := simulator.NewFleet(simulator.FleetOptions{FixturesDir: "../../testdata/fixtures"})
dev, _ := fleet.Add(ctx, simulator.DeviceSpec{Name: "plug", Profile: simulator.ProfileESP8266_1M, Version: "15.6.0"})
// dev.Addr() is a loopback host:port; dev.Commands() records what was sent
```

Fault injection (`simulator.Fault`) covers timeouts, dropped connections, unknown
commands, auth failures, failed downloads, long reboots and wrong-family firmware. Add a
fixture or a fault instead of mocking HTTP by hand.
