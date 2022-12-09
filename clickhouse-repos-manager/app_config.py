#!/usr/bin/env python3

from pathlib import Path

from _vendor.get_robot_token import get_best_robot_token
from flask import Flask


# TODO: tests
def set_deb_config(app: Flask) -> None:
    # See `man 1 reprepro` "conf/distributions" and DEB_REPO_TEMPLATE
    # SNAKE_CASE parameters are converted to PascalCase in g.deb_config
    # app.config["SIGNING_KEY"] is used as SignWith by default
    app.config["DEB_REPO_ORIGIN"] = "ClickHouse"
    app.config["DEB_REPO_LABEL"] = "ClickHouse"
    app.config["DEB_REPO_ARCHITECTURES"] = "amd64 arm64"
    app.config["DEB_REPO_CODENAMES"] = ["lts", "stable"]
    app.config["DEB_REPO_COMPONENTS"] = "main"
    app.config["DEB_REPO_LIMIT"] = -1


# TODO: tests
def set_config(app: Flask) -> None:
    set_deb_config(app)
    app.config["GITHUB_REPOSITORY"] = "ClickHouse/ClickHouse"
    app.config["REPOS_ROOT"] = Path.home() / "r2"
    app.config["S3_BUILDS_BUCKET"] = "clickhouse-builds"
    app.config["S3_TEST_REPORTS_BUCKET"] = "clickhouse-test-reports"
    app.config["S3_URL"] = "https://s3.amazonaws.com"
    app.config["SIGNING_KEY"] = "885E2BDCF96B0B45ABF058453E4AD4719DDE9A38"
    app.config["WORKING_DIR"] = Path.home() / "clickhouse-repository-manager"

    app.config.from_prefixed_env("CHRM")
    # Do not request the token in advance, only on demand
    app.config["GITHUB_TOKEN"] = app.config.get("GITHUB_TOKEN", get_best_robot_token())
