# Firefly Concert — a concurrent light-wall server

## Your task

Imagine a concert where audience members tap their phones to send rectangular bursts of light
to a giant wall. Other clients repeatedly fetch the wall to display it. Implement the HTTP
server that accepts those bursts and returns the current state.

Deliver working code, a command to start it on a specified port, and a short explanation of
your design. Aim for 90 minutes of implementation followed by 20 minutes of discussion. Store
state in memory in one server process.

You do **not** need a UI, authentication, persistence, WebSockets, animation, decay, distributed
storage, or retry deduplication. The challenge is correct shared state under concurrent requests
and fast HTTP responses.

## Your choice of stack is part of the problem

Use any language, runtime, and framework. None is required or recommended, but the choice
matters, and it is yours to make and defend.

**Performance is part of the score.** Speed is 30 of the 100 points, and the grader offers
10,000 requests per second whether or not your server keeps up. How much of that one server
process can absorb depends heavily on the stack underneath your code:

- how many CPU cores can run your request handlers at the same time;
- how much work each request costs before your code even runs (parsing, framework,
  interpreter or runtime overhead, JSON encoding of 4,096 integers);
- whether garbage collection, a shared thread, or queued work adds pauses that show up in p99.

**The stack also changes how hard the problem is.** Where handlers truly run in parallel, you
must protect shared state yourself, and a mistake shows up as torn snapshots or lost updates.
Where handlers run one at a time, correctness is easier to reach, but every request shares
that one thread: a slow snapshot delays everyone behind it, and throughput stops at what one
core can do. A stack that makes the code simple can cap your speed points; a stack that raises
the ceiling can make correctness harder to get right in 90 minutes.

There is no single right answer. Pick deliberately, and be ready to explain what your choice
bought you, what it cost you, and where it limited your score. The interviewer weighs your
result with that tradeoff in mind.

## AI assistance: coding only

You may use an AI coding assistant (Claude Code, Codex, Cursor, Copilot, or similar) to write
code. You may not use it for the parts being evaluated. Those are yours:

- choosing your language, runtime, and framework;
- choosing the synchronization design and explaining why it is correct;
- running the grader and interpreting its output, finding the bottleneck, and choosing
  what to optimize;
- writing `NOTES.md`;
- the discussion afterward.

Before you start, load the supplied rules into your assistant and show the interviewer that
it has picked them up:

| Assistant | Rules location |
|---|---|
| Claude Code | `CLAUDE.md` and `.claude/settings.json` are already in this folder; start it here |
| Codex, Cursor, and others that read `AGENTS.md` | `AGENTS.md` is already in this folder |
| Anything else | paste `AGENTS.md` as the first message or the system prompt |

Direct the assistant the way you would direct a fast pair who types for you. Tell it the
design in your own words ("one lock per wall; copy pixels and version under it; serialize
after unlocking") and it will implement it. Run the grader yourself in your own terminal.
Share your screen for the whole session and submit the assistant's transcript (for example
`/export` in Claude Code) with your code. Using the assistant for a forbidden task counts
against the submission even when the code scores well.

## The model

- Each wall is a separate 64 × 64 grid of integer energy values. All values initially equal 0.
- Coordinates start at `(0,0)` in the top-left. X increases rightward; Y increases downward.
- A wall starts at version 0. Every successful burst increases its version by exactly 1.
- A burst adds its energy to each cell in a rectangle. Values add without wrapping or saturation.
- The server assigns versions. Clients do not send them.
- Walls are independent. Updating one must not change any other.
- The grader keeps all pixel values and versions below 2^31.

## How to run your submission

Provide your startup command. It must accept the port as an argument, for example:

```sh
<your start command> --port 8765
```

Listen on `127.0.0.1` at the specified port. Support HTTP/1.1 persistent connections or later.
Return `Content-Type: application/json` and correctly frame each response (Content-Length or
chunked encoding). JSON whitespace and object-key order do not matter. Return the exact fields
listed below; do not wrap responses in additional objects.

## API: three endpoints

### Create: `POST /walls`

Body:

```json
{"wall_id":"main-stage"}
```

`wall_id` must be a string of 1–64 characters matching `[A-Za-z0-9_-]+`.

For a new ID, create an empty wall and return **201**:

```json
{"wall_id":"main-stage","version":0}
```

For an existing ID return **409**, without resetting the wall:

```json
{"error":"already_exists"}
```

If eight clients create the same ID concurrently, exactly one must get 201 and seven must get 409.

### Update: `POST /walls/{wall_id}/bursts`

Body:

```json
{"x":2,"y":3,"width":2,"height":2,"energy":5}
```

| Field | Required value |
|---|---|
| `x`, `y` | Integer from 0 through 63 |
| `width`, `height` | Integer from 1 through 64 |
| `energy` | Integer from 1 through 100 |

All five fields are required. The rectangle must fit: `x + width <= 64` and
`y + height <= 64`. Reject rectangles that extend beyond the wall; do not clip them.

Add `energy` to cells with `x <= cell_x < x + width` and `y <= cell_y < y + height`.
The example updates exactly `(2,3)`, `(3,3)`, `(2,4)`, and `(3,4)`.

Apply the whole burst and increment the version atomically. Return **200** with the version
assigned to this specific update:

```json
{"version":1}
```

Repeated identical requests count as separate bursts. If N bursts succeed on a new wall, their
returned versions must be exactly the integers 1 through N, each once. Response arrival order
may differ from version order.

### Read: `GET /walls/{wall_id}`

Return **200** with a consistent snapshot:

```text
{"version": INTEGER, "pixels": ARRAY_OF_EXACTLY_4096_NONNEGATIVE_INTEGERS}
```

`pixels` is row-major: index = `y * 64 + x`. Include all zero entries. It is a flat array,
not 64 nested arrays. Never return a sparse map, image, encoded blob, or total energy instead.

The version and all pixels must come from the same state. A concurrent snapshot may show a
burst entirely before or entirely after it is applied, but never part of that burst. A read
started after receiving a successful burst response must include that burst. Sequential reads
on a client cannot move backward in version.

Different walls do not need a shared ordering. A simple per-wall lock is a valid starting point.

## Invalid requests and error precedence

Inputs are UTF-8 JSON objects. Ignore extra object fields. Required numeric fields must be JSON
integer tokens: `1` is valid; `1.0`, `1e0`, `true`, `null`, and `"1"` are invalid. There are no
NaN/Infinity tokens in valid JSON. Wall-specific path IDs use the same valid ID alphabet above.

For the specified routes, apply these rules in order:

1. Unknown route or nonexistent wall: **404**, `{"error":"not_found"}`. For a nonexistent
   wall this takes precedence even if the burst body is malformed.
2. Malformed JSON, non-object body, missing field, invalid type/range, or rectangle out of bounds:
   **400**, `{"error":"invalid_request"}`.
3. Valid creation request with an existing ID: **409**, `{"error":"already_exists"}`.

Every rejected request must leave the state and version unchanged. Missing fields differ from
extra fields: missing required fields are errors, while unknown fields are ignored.
Methods other than the specified GET/POST routes, query parameters, encoded path characters,
HTTP pipelining, chunked request bodies, and bodies larger than 8 KiB are outside the exercise.
The grader sends valid Content-Length headers. Restarting the server clears all state.

## Worked example with exact values

For a fresh wall `demo`:

| Step | Request | Expected response |
|---|---|---|
| 1 | `POST /walls` with `{"wall_id":"demo"}` | 201, `{"wall_id":"demo","version":0}` |
| 2 | Burst `{"x":2,"y":3,"width":2,"height":2,"energy":5}` | 200, `{"version":1}` |
| 3 | Burst `{"x":3,"y":3,"width":1,"height":1,"energy":2}` | 200, `{"version":2}` |
| 4 | `GET /walls/demo` | 200, version 2, exactly the pixels below |
| 5 | Burst `{"x":63,"y":63,"width":2,"height":2,"energy":1}` | 400, `{"error":"invalid_request"}` |
| 6 | `GET /walls/demo` | Exactly the same JSON values as step 4 |

At step 4, pixels 194, 258, and 259 equal 5; pixel 195 equals 7; all other pixels equal 0.
The sum of all pixel values is 22. The full expected JSON is supplied in `example_snapshot.json`.

## Suggested implementation milestones

1. Start an HTTP server. Implement wall creation and a snapshot of 4,096 zeros.
2. Add validation and rectangle updates. Check the worked example and boundary cases.
3. Protect the wall registry against concurrent creation. Protect each wall's pixels and version
   together. One lock per wall is enough for a correct initial implementation.
4. Copy the pixels and version while holding that lock. Serialize the copy after releasing it,
   so slow network responses do not hold the wall lock. Other correct designs are welcome.
5. Run the correctness suite. Fix correctness before optimizing.
6. Run the load benchmark. Measure before changing data structures or concurrency strategy.

Do not assume that your runtime or framework makes shared state atomic, whether it runs
handlers on many threads, on one event loop, or some other way. Explain which operation
establishes the order of concurrent updates and how a snapshot remains internally consistent.

## Run the grader

The grader is a prebuilt program in `grader/bin/`; pick the binary for your machine. It is the
same for every stack. Its source is in `grader/` if you want to read exactly what it checks.
Start your server, then:

```sh
grader/bin/firefly-grader-darwin-arm64 --url http://127.0.0.1:8765 --correctness-only
grader/bin/firefly-grader-darwin-arm64 --url http://127.0.0.1:8765 --runs 1   # quick speed check
grader/bin/firefly-grader-darwin-arm64 --url http://127.0.0.1:8765            # scored: 30 runs
```

Other binaries: `-darwin-amd64`, `-linux-amd64`, `-linux-arm64`, `-windows-amd64.exe`. On macOS,
if Gatekeeper blocks the binary, run `xattr -d com.apple.quarantine grader/bin/*` once.

The grader creates fresh IDs on every run. It tests exact output, validation, creation races,
concurrent snapshots, and final-state correctness. Failures produce a named check and diagnostic.

The load phase offers **10,000 requests per second** for 3 seconds per workload, spread over 64
keep-alive connections, whether or not your server keeps up. It runs two workloads: one shared
wall and four independent walls, each 75% bursts and 25% snapshots. It reports successful
requests/second and read/write latency p50, p95, and p99. When your server falls behind,
requests queue and the wait counts toward latency. Full snapshots and JSON transport count
toward the cost.

Correctness is worth 70 points. Speed is worth 30 and is awarded only if every correctness check
passes, including the checks made under load in every run. A single run earns full speed
points when your server sustains the offered rate in both workloads with read and write p99 at
or under the target (10 ms by default); falling short on either scales that run's points down.

Latency on a shared machine is noisy, so the scored grade repeats the load phase **30 times**
against the same server and your speed points are the **average** of the 30 runs. The report
also shows each run's points, the standard deviation, and the per-workload averages. A server
that keeps up finishes in about 4 minutes; one that falls behind takes longer. Use `--runs 1`
while you iterate. `--rate`, `--duration`, `--connections`, `--p99-target-ms`, and `--runs`
change the load; the interviewer scores with the defaults.

To see a wall, render any snapshot as a picture (the renderer needs Python 3; your server does
not):

```sh
curl -s http://127.0.0.1:8765/walls/demo | python3 visualize_wall.py - -o demo.png
python3 visualize_wall.py --url http://127.0.0.1:8765 --wall demo --crop 0,0,8,8 --scale 40
```

Submit your source, the assistant transcript, and `NOTES.md` written by you: the startup command,
dependencies, why you chose your stack, synchronization, the observed bottleneck, one
optimization you measured, and which code the assistant wrote. Discuss larger walls or more clients afterward; you do not need to
implement a distributed system.
