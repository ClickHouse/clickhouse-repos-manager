#!/usr/bin/env python
from os import path as p
from pathlib import Path
from threading import Lock

from _vendor.github_helper import GitHub, Repository
from flask import current_app, g


def get_update_repo_lock() -> Lock:
    # We must update only one repository at once due indexing
    # and R2 limitations
    if "update_repo_lock" not in g:
        g.update_repo_lock = Lock()

    return g.update_repo_lock


def get_working_dir() -> Path:
    if "working_dir" not in g:
        g.working_dir = Path(current_app.config["WORKING_DIR"])

    return g.working_dir


def get_releases_dir() -> Path:
    if "releases_dir" not in g:
        g.releases_dir = get_working_dir() / "releases"

    return g.releases_dir


def get_gh_client() -> GitHub:
    if "gh_client" not in g:
        g.gh_client = GitHub(current_app.config["GITHUB_TOKEN"])
        g.gh_client.cache_path = get_working_dir() / "github_cache"

    return g.gh_client


def get_gh_repo() -> Repository:
    if "gh_repo" not in g:
        g.gh_repo = get_gh_client().get_repo(current_app.config["REPOSITORY"])

    return g.gh_repo


def get_s3_builds_url() -> str:
    if "s3_builds_url" not in g:
        g.s3_builds_url = p.join(
            current_app.config["S3_URL"], current_app.config["S3_BUILDS_BUCKET"]
        )

    return g.s3_builds_url


def get_s3_reports_url() -> str:
    if "s3_reports_url" not in g:
        g.s3_reports_url = p.join(
            current_app.config["S3_URL"], current_app.config["S3_TEST_REPORTS_BUCKET"]
        )

    return g.s3_reports_url
