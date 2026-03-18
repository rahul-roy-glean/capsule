#!/usr/bin/env python3
"""Minimal GCE Metadata Server Emulator for Vertex AI authentication.

This emulator allows Claude Code to authenticate with Vertex AI when running
on non-GCP environments (AWS). It serves the minimal endpoints required by
google-auth-library.

Required environment variables:
    - GOOGLE_APPLICATION_CREDENTIALS_TOKEN: The OAuth2 access token to serve
    - GCE_METADATA_HOST=127.0.0.1:8181: Redirect google-auth to local emulator
    - GCE_METADATA_IP=127.0.0.1:8181: Required by some Google libraries

The entrypoint script should start this when START_GCE_METADATA_EMULATOR=1.
"""

import http.server
import json
import os
import sys

DEFAULT_PORT = 8181


class MetadataHandler(http.server.BaseHTTPRequestHandler):
    """Handler for GCE metadata server requests."""

    def do_GET(self) -> None:
        """Handle GET requests to metadata endpoints."""
        token = os.environ.get('GOOGLE_APPLICATION_CREDENTIALS_TOKEN', '')

        if self.headers.get('Metadata-Flavor') != 'Google':
            self.send_response(403)
            self.end_headers()
            self.wfile.write(b'Missing Metadata-Flavor: Google header')
            return

        path = self.path.split('?')[0]

        if path == '/computeMetadata/v1/instance/service-accounts/default/token':
            self._send_json(
                {
                    'access_token': token,
                    'expires_in': 3600,
                    'token_type': 'Bearer',
                }
            )
        elif path == '/computeMetadata/v1/instance':
            self._send_json({'zone': 'projects/glean-vertex-ai/zones/us-east5-a'})
        else:
            self._send_text('')

    def _send_json(self, data: dict) -> None:
        self.send_response(200)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Metadata-Flavor', 'Google')
        self.end_headers()
        self.wfile.write(json.dumps(data).encode())

    def _send_text(self, text: str) -> None:
        self.send_response(200)
        self.send_header('Content-Type', 'text/plain')
        self.send_header('Metadata-Flavor', 'Google')
        self.end_headers()
        self.wfile.write(text.encode())

    def log_message(self, format: str, *args) -> None:
        print(f'[gce-metadata-emulator] {args[0]}', file=sys.stderr)


def main() -> None:
    port = int(os.environ.get('GCE_METADATA_EMULATOR_PORT', str(DEFAULT_PORT)))
    server = http.server.HTTPServer(('127.0.0.1', port), MetadataHandler)
    print(f'[gce-metadata-emulator] Running on 127.0.0.1:{port}', file=sys.stderr)
    server.serve_forever()


if __name__ == '__main__':
    main()
