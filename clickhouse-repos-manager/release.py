#!/usr/bin/env python
from os import path as p
from threading import Thread
from typing import List, Optional
import logging

from _vendor.ci_config import CI_CONFIG, BuildConfig
from _vendor.download_helper import download_with_progress, DownloadException
from _vendor.github_helper import check_tag
from github.Commit import Commit
from github.GithubException import UnknownObjectException
from github.GitRelease import GitRelease
from github.GitTag import GitTag
from packages import Packages
from repos import Repos

import context_helper as ch


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
        self.version, self.version_type = version_tag[1:].split("-", 1)
        version_parts = self.version.split(".")
        self.release_branch = ".".join(version_parts[0:2])
        self.additional_binaries = additional_binaries or []  # type: List[str]

        # Preserve variables from the app global context
        self.working_dir = ch.get_working_dir()
        self.release_dir = ch.get_releases_dir() / self.version_tag
        self.repos_root_dir = ch.get_repos_root_dir()
        self.gh_client = ch.get_gh_client()
        self.gh_repo = ch.get_gh_repo()
        self.update_repo_lock = ch.get_update_repo_lock()
        # Set the S3 artifacts download url and test reports upload bucket
        self.s3_builds_url = ch.get_s3_builds_url()
        self.s3_test_reports_bucket = ch.current_app.config["S3_TEST_REPORTS_BUCKET"]

        self._tag = None  # type: Optional[GitTag]
        self._commit = None  # type: Optional[Commit]
        self._git_release = None  # type: Optional[GitRelease]
        self._set_github_objects()

        # The prefix for the commit's builds
        self.builds_prefix = p.join(
            self.s3_builds_url, self.release_branch, self.commit.sha
        )

        # Create release directory here, after the release is checked
        self.release_dir.mkdir(0o750, parents=True, exist_ok=True)

        self._packages = None  # type: Optional[Packages]
        self.repos = Repos(
            self.packages,
            self.repos_root_dir,
            self.version_type,
            *self.additional_version_types,
        )

    def do(self, synchronous: bool):
        """A function to take care of all release steps:
        - Download packages
        - Download additional OS/arch specific binaries
        - Update repositories (w/ or w/o additional LTS):
            - deb
            - rpm
            - tgz
        - Upload all downloaded files as self.release assets
        - Upload logs to self.s3_test_reports_bucket and create a commit status
        for a successful/unsuccessful relese
        - Mark the release as successful if everything is done
        """
        if synchronous:
            self._background_do()
            return
        thread = Thread(target=self._background_do, name=self.version_tag)
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
        """Sets tag, commit and git_release values. Raises ReleaseException if
        any necessary object is not found"""
        _ = self.tag
        _ = self.commit
        _ = self.git_release

    def _background_do(self):
        self.packages.download(False)
        try:
            self.download_binaries()
        except DownloadException:
            # We still don't have built binaries for some older releases,
            # so it's fine to ignore errors here
            logging.warning(
                "Can't download additional binaries: %s",
                ", ".join(self.additional_binaries),
            )

        with self.update_repo_lock:
            self.repos.add_packages()

        print(f"The background task for {self.version_tag} is done")

    def download_binaries(self):
        if not self.additional_binaries:
            return
        for name in self.additional_binaries:
            url = p.join(self.builds_prefix, name, "clickhouse")

            config = CI_CONFIG["build_config"][name]  # type: BuildConfig
            suffix = config.get("static_binary_name", name)
            binary_path = self.release_dir / f"clickhouse-{suffix}"

            if binary_path.exists():
                logging.info("Binary for build %s already exists", name)
                print("Binary for build %s already exists", name)
                continue

            logging.info("Downloading %s to %s", url, binary_path)
            print("Downloading %s to %s", url, binary_path)
            download_with_progress(url, binary_path)

    @staticmethod
    def is_processed(version_tag: str) -> bool:
        releases_dir = ch.get_releases_dir()
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

    @property
    def packages(self) -> Packages:
        """downloaded packages for the release"""
        if self._packages is None:
            self._packages = Packages(
                self.release_dir, self.builds_prefix, self.version
            )

        return self._packages

    @packages.setter
    def packages(self, packages: Packages):
        self._packages = packages

    @property
    def additional_version_types(self) -> List[str]:
        if self.version_type == "lts":
            return ["stable"]
        return []
