package iot

// PingsTable is the TimescaleDB hypertable for raw vehicle telemetry (GPS, fuel, CAN).
// Partitioned on ts; no synthetic id column (see fleet migrations 0010–0011).
const PingsTable = "telemetry_timeseries"
