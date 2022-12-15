#!/usr/bin/env python
from os import path as p
from pathlib import Path
from threading import Lock
from typing import Dict, List, Union, TYPE_CHECKING

import boto3
from flask import current_app, g

from _vendor.github_helper import GitHub, Repository

if TYPE_CHECKING:
    from mypy_boto3_s3 import S3Client
else:
    S3Client = object


DebParam = Union[str, List[str]]

# a global repository lock
repo_lock = Lock()

# TODO: tests
def remove_prefix(s: str, prefix: str) -> str:
    if not s.startswith(prefix):
        return s

    return s[len(prefix) :]


# TODO: tests
def get_deb_config() -> Dict[str, DebParam]:
    prefix = "DEB_REPO_"

    def process_parameter(param: str) -> str:
        name = remove_prefix(param, prefix)
        name = "".join(part.capitalize() for part in name.split("_"))
        return name

    if "deb_repo_config" not in g:
        g.deb_config = {
            process_parameter(key): value
            for key, value in current_app.config.items()
            if key.startswith(prefix)
        }
        g.deb_config["SignWith"] = g.deb_config.get("SignWith", get_signing_key())

    return g.deb_config


# TODO: tests
def get_gh_client() -> GitHub:
    if "gh_client" not in g:
        g.gh_client = GitHub(current_app.config["GITHUB_TOKEN"])
        g.gh_client.cache_path = get_working_dir() / "github_cache"

    return g.gh_client


# TODO: tests
def get_gh_repo() -> Repository:
    if "gh_repo" not in g:
        g.gh_repo = get_gh_client().get_repo(current_app.config["GITHUB_REPOSITORY"])

    return g.gh_repo


# TODO: tests
def get_releases_dir() -> Path:
    if "releases_dir" not in g:
        g.releases_dir = get_working_dir() / "releases"

    return g.releases_dir


# TODO: tests
def get_repos_root_dir() -> Path:
    if "repos_root_dir" not in g:
        g.repos_root_dir = Path(current_app.config["REPOS_ROOT"])

    return g.repos_root_dir


# TODO: tests
def get_s3_builds_url() -> str:
    if "s3_builds_url" not in g:
        g.s3_builds_url = p.join(
            current_app.config["S3_URL"], current_app.config["S3_BUILDS_BUCKET"]
        )

    return g.s3_builds_url


# TODO: tests
def get_s3_client() -> S3Client:
    if "s3_client" not in g:
        g.s3_client = boto3.client("s3")

    return g.s3_client


# TODO: tests
def get_s3_reports_bucket() -> str:
    if "s3_reports_bucket" not in g:
        g.s3_reports_bucket = current_app.config["S3_TEST_REPORTS_BUCKET"]

    return g.s3_reports_bucket


# TODO: tests
def get_s3_reports_url() -> str:
    if "s3_reports_url" not in g:
        g.s3_reports_url = p.join(
            current_app.config["S3_URL"], current_app.config["S3_TEST_REPORTS_BUCKET"]
        )

    return g.s3_reports_url


# TODO: tests
def get_signing_key() -> str:
    if "signing_key" not in g:
        g.signing_key = current_app.config["SIGNING_KEY"]

    return g.signing_key


# TODO: tests
def get_update_repo_lock() -> Lock:
    # We must update only one repository at once due indexing
    # and R2 limitations
    if "update_repo_lock" not in g:
        g.update_repo_lock = repo_lock

    return g.update_repo_lock


# TODO: tests
def get_working_dir() -> Path:
    if "working_dir" not in g:
        g.working_dir = Path(current_app.config["WORKING_DIR"])

    return g.working_dir


class ContextHelper:
    def __init__(self):
        """The class to preserve all contexts for the thread exetucing"""
        self.working_dir = get_working_dir()
        self.releases_dir = get_releases_dir()
        self.repos_root_dir = get_repos_root_dir()
        self.deb_config = get_deb_config()
        self.gh_client = get_gh_client()
        self.gh_repo = get_gh_repo()
        self.update_repo_lock = get_update_repo_lock()
        # Set the S3 artifacts download url and test reports upload bucket
        self.s3_builds_url = get_s3_builds_url()
        self.s3_client = get_s3_client()
        self.s3_test_reports_bucket = get_s3_reports_bucket()
        self.signing_key = get_signing_key()
