# Personal PoRep visibility preference

The PoRep sector table initially hides sectors that are neither failed nor past
SDR and have no current SDR owner. The checkbox restores the full server result.
This replaces the historical shared-API filtering with a presentation-only rule:
the API still returns every sector, and page counts explicitly distinguish shown,
hidden, and returned rows. Existing waiting-stage counters use the full result.

`SDROwned` is additive owner telemetry from the same SQL statement as the sector
snapshot. It is deliberately distinct from execution-start telemetry and from
the older stage-dependent `StartedSDR`. An older response without this field is
left visible rather than treated as proof of unowned state. No pipeline state,
task, shared API visibility, or history is modified by toggling the preference.

The personal page sets `hide-pending-sdr` on the common component. This keeps
the initial personal choice without maintaining a second filtering algorithm.
