#!/usr/bin/env python
from os import path as p
from threading import Thread
from typing import List, Optional

from _vendor.github_helper import check_tag
from _vendor.packages import Packages
from context_helper import (
    current_app,
    get_gh_client,
    get_gh_repo,
    get_releases_dir,
    get_s3_builds_url,
    get_update_repo_lock,
    get_working_dir,
)
from github.Commit import Commit
from github.GithubException import UnknownObjectException
from github.GitRelease import GitRelease
from github.GitTag import GitTag


class ReleaseException(BaseException):
    pass


class Release:
    def __init__(self, version_tag: str, additional_binaries: List[str] = None):
        # The class init and static methods depend on global app context and
        # must be executed only during requests
        #
        # *do* can be executed in background
        self.version_tag = version_tag
        self._verify_version()
        self.version = version_tag[1:].split("-", 1)[0]
        version_parts = self.version.split(".")
        self.release_branch = ".".join(version_parts[0:2])
        self.additional_binaries = additional_binaries or []  # type: List[str]

        # Preserve variables from the app global context
        self.working_dir = get_working_dir()
        self.release_dir = get_releases_dir() / self.version_tag
        self.gh_client = get_gh_client()
        self.gh_repo = get_gh_repo()
        self.update_repo_lock = get_update_repo_lock()

        self._tag = None  # type: Optional[GitTag]
        self._commit = None  # type: Optional[Commit]
        self._git_release = None  # type: Optional[GitRelease]
        self._set_github_objects()

        # Create release directory here, after the release is checked
        self.release_dir.mkdir(0o750, parents=True, exist_ok=True)

        # Set the S3 artifacts download url and test reports upload bucket
        self.s3_builds_url = get_s3_builds_url()
        self.s3_test_reports_bucket = current_app.config["S3_TEST_REPORTS_BUCKET"]

    def do(self, foreground: bool):
        if foreground:
            self._background_do()
            return
        thread = Thread(target=self._background_do)
        thread.start()

    def _verify_version(self):
        try:
            check_tag(self.version_tag)
        except ValueError as exc:
            raise ReleaseException(
                f"Version '{self.version_tag}' does not match the "
                "v11.2.3.44-{lts,prestable,stable,testing}",
            ) from exc

    def _set_github_objects(self):
        _ = self.tag
        _ = self.commit
        _ = self.git_release

    def _background_do(self):
        self.download_packages()
        self.download_binaries()

    def download_packages(self):
        url_prefix = p.join(self.s3_builds_url, self.release_branch, self.commit.sha)
        packages = Packages(self.release_dir, url_prefix, self.version)
        packages.download(False)

    def download_binaries(self):
        pass

    @staticmethod
    def is_processed(version_tag: str) -> bool:
        releases_dir = get_releases_dir()
        return (releases_dir / version_tag / "finished").exists()

    @property
    def tag(self) -> GitTag:
        if self._tag is None:
            try:
                # The first tag is received as ref/tags/version_tag, and is highly
                # likely annotated tag. That's why we need to get the tag from ref.sha
                ref = self.gh_repo.get_git_ref(f"tags/{self.version_tag}")
                self._tag = self.gh_repo.get_git_tag(ref.object.sha)
            except UnknownObjectException as exc:
                raise ReleaseException(
                    f"Tag for version_tag '{self.version_tag}' is not found"
                ) from exc

        return self._tag

    @property
    def commit(self) -> Commit:
        if self._commit is None:
            try:
                self._commit = self.gh_repo.get_commit(self.tag.object.sha)
            except UnknownObjectException as exc:
                raise ReleaseException(
                    f"Tag for version_tag '{self.version_tag}' is not found"
                ) from exc

        return self._commit

    @property
    def git_release(self) -> GitRelease:
        if self._git_release is None:
            try:
                self._git_release = self.gh_repo.get_release(self.version_tag)
            except UnknownObjectException as exc:
                raise ReleaseException(
                    f"Release for version_tag '{self.version_tag}' is not found"
                ) from exc

        return self._git_release
