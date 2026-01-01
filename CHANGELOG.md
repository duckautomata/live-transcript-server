# latest
Using version [2.1](#21-2026-01-01)

# Major version 2
Using version [2.1](#21-2026-01-01)

# Major version 1
Using version [1.6](#16-2025-12-18)

## 2.1 (2026-01-01)
**Changes**
- Extract first frame of media for every line
- Added /frame endpoint

## 2.0 (2025-12-30)
**Changes**
- Converted storage to sql database
- Changed api routes to have a better name
- Media is now uploaded through /media. This is to enure that transcript data is not throttled when network is congested.
- Added tests
- Changed websocket messages to json format to better handle changes and reduce confusion/mistakes.

## 1.6 (2025-12-18)
**Changes**
- Updated refresh call to ui to include upload time (ms) and processing start time (UnixTimestampMs).

## 1.5 (2025-11-30)
**Changes**
- Changed audio from mp3 to m4a
- Simplified clipping since we generate a unique merge file per clip
- Changed how creds are used. Now use api key header instead.

## 1.4 (2025-10-04)
**Changes**
- Added Prometheus metrics and converted runtime to use Docker compose so that it starts up with the Prometheus and Grafana server.
- Remove server stats since it is no longer needed.
- Upgrading dependencies

## 1.3 (2025-09-28)
**Changes**
- Upgrading dependencies

## 1.2 (2025-06-14)
**Changes**
- Added server stats and log them every 12 hours from when the server started up.

## 1.1 (2025-06-13)
**Changes**
- Increased max clip size to 20 lines
- Fixed `scripts/build.sh` to use sudo if docker requires it
- Updated dependencies

## 1.0 (2025-05-26)
Initial version.