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
from types import SimpleNamespace
from unittest import mock

import pytest

from kserve_storage.s3_credentials import (
    _fetch_jwt_svid,
    configure_spiffe_credentials,
)


def test_spiffe_credentials_are_disabled_without_audience(monkeypatch):
    monkeypatch.delenv("SPIFFE_AUDIENCE", raising=False)

    with mock.patch("kserve_storage.s3_credentials.boto3.client") as sts_client:
        configure_spiffe_credentials()

    sts_client.assert_not_called()


def test_configure_spiffe_credentials():
    environment = {
        "SPIFFE_AUDIENCE": "seaweedfs-s3",
        "SPIFFE_ENDPOINT_SOCKET": "unix:///spiffe-workload-api/spire-agent.sock",
        "AWS_ROLE_ARN": "arn:aws:iam::prokube:role/dev-one-s3-edit",
        "AWS_ROLE_SESSION_NAME": "kserve-storage-initializer",
        "AWS_ENDPOINT_URL_STS": (
            "http://seaweedfs-s3.seaweedfs.svc.cluster.local:8333"
        ),
        "AWS_DEFAULT_REGION": "us-east-1",
    }
    sts = mock.MagicMock()
    sts.assume_role_with_web_identity.return_value = {
        "Credentials": {
            "AccessKeyId": "temporary-access-key",
            "SecretAccessKey": "temporary-secret-key",
            "SessionToken": "temporary-session-token",
        }
    }
    with (
        mock.patch.dict(os.environ, environment, clear=True),
        mock.patch(
            "kserve_storage.s3_credentials._fetch_jwt_svid",
            return_value="jwt-svid",
        ) as fetch_jwt,
        mock.patch(
            "kserve_storage.s3_credentials.boto3.client", return_value=sts
        ) as sts_client,
    ):
        configure_spiffe_credentials()
        assert os.getenv("AWS_ACCESS_KEY_ID") == "temporary-access-key"
        assert os.getenv("AWS_SECRET_ACCESS_KEY") == "temporary-secret-key"
        assert os.getenv("AWS_SESSION_TOKEN") == "temporary-session-token"

    fetch_jwt.assert_called_once_with("seaweedfs-s3")
    sts_client.assert_called_once_with(
        "sts",
        endpoint_url="http://seaweedfs-s3.seaweedfs.svc.cluster.local:8333",
        region_name="us-east-1",
    )
    sts.assume_role_with_web_identity.assert_called_once_with(
        RoleArn="arn:aws:iam::prokube:role/dev-one-s3-edit",
        RoleSessionName="kserve-storage-initializer",
        WebIdentityToken="jwt-svid",
    )


def test_fetch_jwt_svid_retries(monkeypatch):
    client = mock.MagicMock()
    client.fetch_jwt_svid.side_effect = [
        RuntimeError("workload API is not ready"),
        SimpleNamespace(token="jwt-svid"),
    ]
    workload_api_client = mock.MagicMock()
    workload_api_client.return_value.__enter__.return_value = client
    monkeypatch.setattr(
        "kserve_storage.s3_credentials.WorkloadApiClient", workload_api_client
    )
    sleep = mock.MagicMock()
    monkeypatch.setattr("kserve_storage.s3_credentials.time.sleep", sleep)

    assert _fetch_jwt_svid("seaweedfs-s3") == "jwt-svid"
    assert client.fetch_jwt_svid.call_count == 2
    sleep.assert_called_once_with(0.5)


@pytest.mark.parametrize(
    "endpoint",
    [
        "seaweedfs-s3:8333",
        "ftp://seaweedfs-s3:8333",
        "http://user@seaweedfs-s3:8333",
        "http://seaweedfs-s3:8333/sts",
        "http://seaweedfs-s3:8333?query=true",
    ],
)
def test_configure_spiffe_credentials_rejects_invalid_sts_endpoint(
    monkeypatch, endpoint
):
    monkeypatch.setenv("SPIFFE_AUDIENCE", "seaweedfs-s3")
    monkeypatch.setenv("SPIFFE_ENDPOINT_SOCKET", "unix:///tmp/spire-agent.sock")
    monkeypatch.setenv("AWS_ROLE_ARN", "arn:aws:iam::prokube:role/dev-one-s3-edit")
    monkeypatch.setenv("AWS_ENDPOINT_URL_STS", endpoint)

    with (
        mock.patch(
            "kserve_storage.s3_credentials._fetch_jwt_svid",
            return_value="jwt-svid",
        ),
        pytest.raises(RuntimeError, match="invalid AWS_ENDPOINT_URL_STS"),
    ):
        configure_spiffe_credentials()
