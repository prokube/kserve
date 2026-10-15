# Copyright 2026 The KServe Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#    http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

import os
import time
from urllib.parse import urlparse

import boto3
from spiffe import WorkloadApiClient

from kserve_storage.logging import logger

_SPIFFE_JWT_FETCH_ATTEMPTS = 20
_SPIFFE_JWT_FETCH_INTERVAL = 0.5


def _required_env(name: str) -> str:
    value = os.getenv(name)
    if not value:
        raise RuntimeError(f"{name} must be set for SPIFFE authentication")
    return value


def _fetch_jwt_svid(audience: str) -> str:
    error = None
    for attempt in range(1, _SPIFFE_JWT_FETCH_ATTEMPTS + 1):
        try:
            with WorkloadApiClient() as client:
                return client.fetch_jwt_svid(audience={audience}).token
        except Exception as exc:
            error = exc
            if attempt < _SPIFFE_JWT_FETCH_ATTEMPTS:
                time.sleep(_SPIFFE_JWT_FETCH_INTERVAL)

    raise RuntimeError(
        "failed to fetch SPIFFE JWT-SVID after "
        f"{_SPIFFE_JWT_FETCH_ATTEMPTS} attempts: {error}"
    ) from error


def _sts_endpoint() -> str | None:
    endpoint = os.getenv("AWS_ENDPOINT_URL_STS")
    if not endpoint:
        return None

    parsed = urlparse(endpoint)
    if (
        parsed.scheme not in ("http", "https")
        or not parsed.netloc
        or parsed.username is not None
        or parsed.password is not None
        or parsed.path not in ("", "/")
        or parsed.params
        or parsed.query
        or parsed.fragment
    ):
        raise RuntimeError(f"invalid AWS_ENDPOINT_URL_STS {endpoint!r}")
    return endpoint.rstrip("/")


def configure_spiffe_credentials() -> None:
    """Exchange a SPIFFE JWT-SVID for temporary AWS credentials when enabled."""
    audience = os.getenv("SPIFFE_AUDIENCE")
    if not audience:
        return

    _required_env("SPIFFE_ENDPOINT_SOCKET")
    role_arn = _required_env("AWS_ROLE_ARN")
    role_session_name = os.getenv("AWS_ROLE_SESSION_NAME", "kserve-storage-initializer")

    token = _fetch_jwt_svid(audience)
    sts = boto3.client(
        "sts",
        endpoint_url=_sts_endpoint(),
        region_name=os.getenv("AWS_DEFAULT_REGION", "us-east-1"),
    )
    response = sts.assume_role_with_web_identity(
        RoleArn=role_arn,
        RoleSessionName=role_session_name,
        WebIdentityToken=token,
    )
    credentials = response["Credentials"]
    os.environ.update(
        {
            "AWS_ACCESS_KEY_ID": credentials["AccessKeyId"],
            "AWS_SECRET_ACCESS_KEY": credentials["SecretAccessKey"],
            "AWS_SESSION_TOKEN": credentials["SessionToken"],
        }
    )
    logger.info("Configured temporary S3 credentials using SPIFFE workload identity")
