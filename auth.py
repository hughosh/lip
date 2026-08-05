#!/usr/bin/env python3
"""Kalshi RSA-PSS request signing.

Credentials are referenced by path and never logged. The private key stays in
memory only as a cryptography key object; nothing here prints or transmits it.

  ~/.kalshi/kalshi.pem  RSA private key (mode 600)
  ~/.kalshi/env         KALSHI_API_KEY_ID=...
"""
from __future__ import annotations

import base64
import os
import time
from pathlib import Path

from cryptography.hazmat.primitives import hashes, serialization
from cryptography.hazmat.primitives.asymmetric import padding

KEY_PATH = Path(os.path.expanduser("~/.kalshi/kalshi.pem"))
ENV_PATH = Path(os.path.expanduser("~/.kalshi/env"))

REST_HOST = "https://api.elections.kalshi.com"
WS_URL = "wss://external-api-ws.kalshi.com/trade-api/ws/v2"
WS_PATH = "/trade-api/ws/v2"


def _read_env() -> dict[str, str]:
    out: dict[str, str] = {}
    for line in ENV_PATH.read_text().splitlines():
        line = line.strip()
        if line and not line.startswith("#") and "=" in line:
            k, v = line.split("=", 1)
            out[k.strip()] = v.strip().strip("'\"")
    return out


class Signer:
    """Signs Kalshi requests. One instance per process is plenty."""

    def __init__(self) -> None:
        self.key_id = _read_env()["KALSHI_API_KEY_ID"]
        self._key = serialization.load_pem_private_key(
            KEY_PATH.read_bytes(), password=None
        )

    def headers(self, method: str, path: str) -> dict[str, str]:
        """Auth headers for `method path`.

        `path` is the full API path with no query string, e.g.
        /trade-api/v2/portfolio/balance or /trade-api/ws/v2.
        """
        ts = str(int(time.time() * 1000))
        sig = self._key.sign(
            f"{ts}{method.upper()}{path}".encode(),
            padding.PSS(
                mgf=padding.MGF1(hashes.SHA256()),
                salt_length=padding.PSS.DIGEST_LENGTH,
            ),
            hashes.SHA256(),
        )
        return {
            "KALSHI-ACCESS-KEY": self.key_id,
            "KALSHI-ACCESS-TIMESTAMP": ts,
            "KALSHI-ACCESS-SIGNATURE": base64.b64encode(sig).decode(),
        }

    def ws_headers(self) -> dict[str, str]:
        return self.headers("GET", WS_PATH)
