#!/usr/bin/env python3
"""Process-scoped, opaque CONNECT tunnel for the q01 websocket drill.

Set HTTPS_PROXY=http://127.0.0.1:<status.port> only on the observer process.
Set NO_PROXY for every other required host. Creating --trigger closes one active
websocket tunnel; the next connection can then reconnect normally. This helper
never reads TLS contents and never records request headers or payload bytes.
"""

import argparse
import asyncio
import json
import os
from pathlib import Path
import time


WS_HOST = "external-api-ws.kalshi.com"


def record(path: Path, event: str, **fields: object) -> None:
    row = {"time_utc": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
           "event": event, **fields}
    with path.open("a", encoding="utf-8") as out:
        out.write(json.dumps(row, sort_keys=True) + "\n")


async def pump(reader: asyncio.StreamReader, writer: asyncio.StreamWriter) -> None:
    try:
        while data := await reader.read(65536):
            writer.write(data)
            await writer.drain()
    except (ConnectionError, OSError):
        pass


async def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--status", required=True, type=Path)
    parser.add_argument("--trigger", required=True, type=Path)
    parser.add_argument("--log", required=True, type=Path)
    args = parser.parse_args()
    if args.trigger.exists():
        parser.error("trigger file already exists; remove it before starting")
    active: set[tuple[asyncio.StreamWriter, asyncio.StreamWriter]] = set()

    async def handle(client_r: asyncio.StreamReader,
                     client_w: asyncio.StreamWriter) -> None:
        upstream_w = None
        try:
            first = await asyncio.wait_for(client_r.readline(), timeout=10)
            # Only CONNECT to the one pinned exchange host is permitted. Read
            # headers to finish HTTP parsing, but do not store or log them.
            if first != b"CONNECT " + WS_HOST.encode() + b":443 HTTP/1.1\r\n":
                client_w.write(b"HTTP/1.1 403 Forbidden\r\n\r\n")
                await client_w.drain()
                record(args.log, "rejected_connect")
                return
            total = len(first)
            while True:
                line = await asyncio.wait_for(client_r.readline(), timeout=10)
                total += len(line)
                if total > 16384 or not line:
                    raise ValueError("incomplete or oversized CONNECT headers")
                if line == b"\r\n":
                    break
            upstream_r, upstream_w = await asyncio.wait_for(
                asyncio.open_connection(WS_HOST, 443), timeout=10)
            client_w.write(b"HTTP/1.1 200 Connection Established\r\n\r\n")
            await client_w.drain()
            pair = (client_w, upstream_w)
            active.add(pair)
            record(args.log, "tunnel_open", host=WS_HOST)
            flows = [asyncio.create_task(pump(client_r, upstream_w)),
                     asyncio.create_task(pump(upstream_r, client_w))]
            done, pending = await asyncio.wait(flows, return_when=asyncio.FIRST_COMPLETED)
            for task in pending:
                task.cancel()
            await asyncio.gather(*flows, return_exceptions=True)
            active.discard(pair)
            record(args.log, "tunnel_closed", host=WS_HOST)
        except (asyncio.TimeoutError, ConnectionError, OSError, ValueError) as exc:
            record(args.log, "tunnel_error", kind=type(exc).__name__)
        finally:
            client_w.close()
            if upstream_w is not None:
                upstream_w.close()
            await asyncio.gather(client_w.wait_closed(), return_exceptions=True)
            if upstream_w is not None:
                await asyncio.gather(upstream_w.wait_closed(), return_exceptions=True)

    async def watch_trigger() -> None:
        while True:
            if args.trigger.exists() and active:
                # Exactly one active tunnel is expected for the one-market
                # observer. Drop only one, then consume the trigger so a fresh
                # reconnect stays up.
                pair = next(iter(active))
                active.discard(pair)
                pair[0].close()
                pair[1].close()
                args.trigger.unlink()
                record(args.log, "forced_disconnect", host=WS_HOST)
            await asyncio.sleep(0.1)

    server = await asyncio.start_server(handle, "127.0.0.1", 0, limit=16384)
    port = server.sockets[0].getsockname()[1]
    temporary = args.status.with_suffix(args.status.suffix + ".tmp")
    temporary.write_text(json.dumps({"host": "127.0.0.1", "port": port,
                                     "target": WS_HOST}) + "\n", encoding="utf-8")
    os.replace(temporary, args.status)
    record(args.log, "listening", host="127.0.0.1", port=port)
    async with server:
        await asyncio.gather(server.serve_forever(), watch_trigger())


if __name__ == "__main__":
    asyncio.run(main())
