# latest
Using version [1.4](#14-2025-10-04)

# Major version 1
Using version [1.4](#14-2025-10-04)

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