# live-transcript-server
A Go WebSocket server that receives transcript data from worker and then propagates it out to all web clients.

## Overview

_System_

- **[Live Transcript System](#live-transcript-system)**
- **[Server System](#server-system)**
- **[Flows](#flows)**
- **[Events](#events)**
- **[Media Clipping](#media-clipping)**

_Development_

- **[Tech Used](#tech-used)**
- **[Requirements](#requirements)**
- **[Running Source Code](#running-source-code)**
- **[Debugging/Logging](#debugginglogging)**

_Docker_
- **[Host Requirements](#host-requirements)**
- **[Version Guide](#version-guide)**
- **[Running with Docker](#running-with-docker)**

## System

### Live Transcript System
Live Transcript is a system that contains three programs:
- Worker: [live-transcript-worker](https://github.com/duckautomata/live-transcript-worker)
- Server: [live-transcript-server](https://github.com/duckautomata/live-transcript-server)
- Client: [live-transcript](https://github.com/duckautomata/live-transcript)

All three programs work together to transcribe a livestream for us to use in real-time.
- Worker will process a livestream, transcribe the audio, and then upload the results to the server.
- Server (this) acts as a cache layer between Worker and Client. It will store the current transcript. Once it receives a new transcript line, it will be broadcast to all connected clients.
- Client is the UI that renders the transcript for us to use.

### Server System

The server is a straightforward Go WebSocket server that has three main functions:
1. Store transcript data for any given key
2. Upon a new connection, send them the current transcript data
3. Upon a new transcript line, add the line to the local transcript and broadcast the line to every client.

Some additional functions it does is
- store media data to file and serve them upon request. Either for a specific line or merge multiple lines into one media file.

The main functions are explained in [Flows](#flows) and [Events](#events). The media data is explained in [Media Clipping](#media-clipping).

### Flows
#### From worker
Stream Start
- worker calls /{key}/activate?data...
- Server updates data with the new stream and broadcasts details to all clients

Stream is running
- Worker calls /{key}/update with the new transcript line and media.
- if the received line has the correct ID (next in line), the server adds the new line to its local data, saves the media to file, then broadcasts the new line to all clients
- else, that means the Server and Worker are out of sync. To fix this, the server responds with 409, telling the worker to call to upload, which will reset the server state to the client's current state.
    + We expect there to be missing data if the Server and Client go out of sync. But we'll let the Client figure that out on how to proceed.

Fixing out-of-sync issue
- worker calls /{key}/upload with its entire current state.
- Server resets its state with the data the worker provided.
- The server broadcasts the last line of the new transcript to every client.
    + This will cause every client to go out of sync with the server. But we're OK with this since the client will decide how to proceed. We don't want to resync every client if some are unused.

Stream ends
- worker calls /{key}/deactivate?data...
- server updates live to false and broadcasts details to all clients

#### From client
New connection
- Client calls /ws/{key}
- connection turns into a WebSocket
- hardRefresh(conn) is called, and the current state is sent to the client

Missing data
- Currently, there is no support to update any missing data

Hard Refresh
The client wants to resync the entire state.
- Currently, the only support for hard refresh is for the client to close the connection and open a new one.

### Events
#### Server to Client
- refresh to add a new line
- hardrefresh to update the entire state
- newstream when a new stream starts. Reset client state.
- status when the status of a current stream changes.
- error when there is some error with the server.

#### Message structure
All messages will start with `![]` and split up the parts of the message with `\n` except for hardrefresh, which will send the entire transcript data as a JSON for simplicity.

The goal of this message structure is to minimize data being sent between the server and the client. It also minimizes the computation time needed to generate the message. JSON is very slow.
- ![]refresh\n{timestamp_1}\n{text_1}\n{timestamp_2}\n{text_2} ...
- ![]newstream\n{streamId}\n{streamTitle}\n{startTimeUnix}\n{mediaType}\n{isLive}
- ![]status\n{streamId}\n{streamTitle}\n{isLive}
- ![]error\n{errorType}\n{message}

### Media Clipping

Because we don't know what type of media the worker will send to us (MPEG-TS audio, DASH video, etc.), the server treats the media received from the worker as untrusted binary data (`.raw`) and uses the `mediaType` variable to denote what type of media it is. It can be
- none if we are not sending any data
- audio
- video

When the stream starts, the worker will tell the server what media type it will send for that stream. Once mediaType is set, it cannot change for the entirety of that stream.
When the server receives new media data, it will
1. save the untrusted data as a `.raw` file under tmp/{key}/media using the line id as the file name.
2. use FFmpeg to extract the audio from it and save it in a `.mp3` using the same line ID as the file name.
3. If FFmpeg fails to extract the audio, the server treats the untrusted data as corrupted and deletes it.

When the client requests the audio for a specific line, the server will use the `.mp3` file, which is guaranteed to be a valid audio file.

#### Clipping
When the client requests a clip (either audio or video) between and including two id's, the server will
1. merge all `.raw` files in that range into a single `.raw` file
    + Because this is the unmodified stream data, doing it this way ensures there are no gaps between lines. You would get gaps if you tried to merge the `.mp3` files since the conversion is not perfect.
2. use FFmpeg to convert the `.raw` file into the requested media type file (either `.mp3` for audio or `.mp4` for video)
3. delete the merged `.raw` file and respond with that new file. Which is guaranteed to be a valid media file.

## Development

### Tech Used
- Go 1.24
- FFmpeg

### Requirements
- [Go](https://go.dev/doc/install)
- FFmpeg
- Any OS

### Running Source Code

**NOTE**: This is only required to run the source code. If you only want to run it and not develop it, then check out the [Docker seciton](#docker)

1. Download and install Go and FFmpeg
2. Referencing `config-example.yaml`, create `config.yaml` and add your specific configurations.
5. Download dependencies `go mod download`

When all of that is done, you can run `scripts/run.sh` (or just `go run ./cmd/web/` from the root directory) to start live-transcript-server.

### Debugging/Logging

Logging is set up for the entire program, and everything should be logged. The console will print info and higher logs (everything but debug). On startup, a log file under `tmp/` will be created and will contain every log. In the event of an error, check this log file to see what went wrong.


## Docker

### Host Requirements
- Any OS
- Docker

If it has Docker, it can run this.

### Version Guide
Uses an x.y major.minor version standard.

Major version is used to denote any API/breaking changes.

Minor version is used to denote any code/dependency changes that do not break anything.

Tags:
- `latest` will always be the most recent image.
- `x` will be the latest x major version image. Meaning, if the tag is `2` and the latest `2.y` image is `2.10`, then `2` will use the `2.10` image. When a new `2.11` image is created, then the tag `2` will use that new image.
- `x.y` will be a specific image.

The major version between Worker and Server _should_ remain consistent.

You can view all tags on [Dockerhub](https://hub.docker.com/r/duckautomata/live-transcript-server/tags)

### Running with Docker
The easiest way to run the docker image is to
1. clone this repo locally
2. create `config.yaml` from the example config file, adding in your specific configurations.
3. then run `./docker/start.sh`

If there are permission errors and the container cannot write to tmp/, then you first need to run `sudo chmod -R 777 tmp` to give the container permissions.

Depending on your use case, you can change the configuration variables in `start.sh` to match your needs.

Logs and current state are stored in the `tmp/` folder outside the container. Because of this, state is not lost on restart.

**Note**: the docker container and the source code use the same `tmp/` folder to store runtime data. Because of this, you are required to run either or, but not both. If you want to run both development and a docker image, then use separate folders.

### Viewing Metrics
1. Go to `http://<servers ip address>:8090` to view the Prometheus webpage.
    - Click on Status and go to Targets to verify that the server target is working and up.
    - If it is not, then that means Prometheus cannot reach the server.
    - To fix this, edit the target in the `prometheus.yaml` file and make sure it has a reachable ip address.
2. Go to `http://<servers ip address>:3000` to view the Grafana webpage. 
    - Log in using admin/admin and reset the password.
    - Here, you can add Prometheus as a datasource and start creating a dashboard. Make sure to use the `8090` port address.

Important to note that Prometheus's data will reset every time you stop/start it. And Grafana's data/dashboards will reset if you run `./cleanup.sh`. So make sure that you have the your dashboards back up.
