#!/usr/bin/env python
from os import path as p
from pathlib import Path
from queue import Queue
from threading import Thread
from typing import Final, List, Optional, Literal
import logging

from github.Commit import Commit
from github.GithubException import GithubException, UnknownObjectException
from github.GitRelease import GitRelease
from github.GitReleaseAsset import GitReleaseAsset
from github.GitTag import GitTag

from _vendor.ci_config import CI_CONFIG, BuildConfig
from _vendor.download_helper import download, DownloadException
from _vendor.github_helper import check_tag
from packages import Packages
from repos import Repos
from context_helper import ContextHelper, get_releases_dir


logger = logging.getLogger(__name__)

SUCCESS: Final = "success"
FAILURE: Final = "failure"
STATUS = Literal["success", "failure"]


class ReleaseException(BaseException):
    pass


class Release:
    LOG_NAME = "publish-release.txt"

    def __init__(
        self,
        version_tag: str,
        context_helper: ContextHelper,
        logger: logging.Logger,
        additional_binaries: Optional[List[str]] = None,
    ):
        # The class init and static methods depend on global app context and
        # must be executed only during requests
        #
        # *do* can be executed in background
        self.version_tag = version_tag
        self.logger = logger
        self._log_file = self.log_file(version_tag)
        self._verify_version()
        self.version, self.version_type = version_tag[1:].split("-", 1)
        version_parts = self.version.split(".")
        self.release_branch = ".".join(version_parts[0:2])
        self.additional_binaries = additional_binaries or []  # type: List[str]
        self.exceptions = Queue()  # type: Queue[ReleaseException]
        self.ch = context_helper

        self.release_dir = self.ch.releases_dir / self.version_tag
        self._tag = None  # type: Optional[GitTag]
        self._commit = None  # type: Optional[Commit]
        self._assets = []  # type: List[GitReleaseAsset]
        self.logger.info(
            "The release is created for tag %s and commit %s", self.tag, self.commit
        )

        self._gh_release = None  # type: Optional[GitRelease]
        _ = self.gh_release  # check if release is created

        # The prefix for the commit's builds
        self.builds_prefix = p.join(
            self.ch.s3_builds_url, self.release_branch, self.commit.sha
        )

        # Create release directory here, after the release is checked
        self.release_dir.mkdir(0o750, parents=True, exist_ok=True)

        self._packages = None  # type: Optional[Packages]
        self.repos = Repos(
            self.packages,
            self.ch.repos_root_dir,
            self.ch.deb_config,
            self.ch.signing_key,
            self.version_type,
            *self.additional_version_types,
            logger=self.logger,
        )

    def do(self, synchronous: bool) -> Optional[Thread]:
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
            return None
        thread = Thread(target=self._background_do, name=self.version_tag)
        thread.start()
        return thread

    def _verify_version(self) -> None:
        try:
            check_tag(self.version_tag)
        except ValueError as exc:
            raise ReleaseException(
                f"Version '{self.version_tag}' does not match the "
                "v11.2.3.44-{lts,prestable,stable,testing}",
            ) from exc

    def _background_do(self) -> None:
        try:
            self.packages.download(False, logger=self.logger)

            # FIXME: think about checking if the SAME release is launched twice

            if self.ch.update_repo_lock.locked():
                self.logger.info(
                    "The repositories are already updating by another process, waiting"
                )
            with self.ch.update_repo_lock:
                self.repos.add_packages()

            for package in self.packages.all():
                self.logger.info(
                    "Uploading %s to the release assets", package.path.name
                )
                self.upload_asset(package.path)

            self.process_additional_binaries()
            self.mark_finished(SUCCESS)

        except (BaseException, Exception) as e:
            exc = ReleaseException(
                f"exception occured during release {self.version_tag}"
            ).with_traceback(e.__traceback__)
            self.logger.exception(exc)
            self.exceptions.put(exc)
            self.mark_finished(FAILURE)
            raise

        self.logger.info("The background task for %s is done", self.version_tag)

    def process_additional_binaries(self) -> None:
        if not self.additional_binaries:
            return
        for name in self.additional_binaries:
            url = p.join(self.builds_prefix, name, "clickhouse")

            config = CI_CONFIG["build_config"][name]  # type: BuildConfig
            suffix = config.get("static_binary_name", name)
            binary_path = self.release_dir / f"clickhouse-{suffix}"

            if binary_path.exists():
                self.logger.info("Binary for build %s already exists", name)
                continue

            self.logger.info("Downloading %s to %s", url, binary_path)
            try:
                download(url, binary_path)
            except DownloadException:
                # We still don't have built binaries for some older releases,
                # so it's fine to ignore errors here
                self.logger.warning(
                    "Can't download additional binaries: %s",
                    ", ".join(self.additional_binaries),
                )
                continue
            self.logger.info("Upload %s to the release assets", binary_path.name)
            self.upload_asset(binary_path)

    def upload_asset(self, path: Path) -> None:
        # The logic that upload_asset() checks the existing on its own doesn't
        # work, see https://github.com/PyGithub/PyGithub/issues/2385
        # We must check the release for existing assets.
        # The reasonable approach is getting assets once on demand at the
        # upload start
        if [asset for asset in self.assets if path.name == asset.name]:
            self.logger.info(
                "Asset %s already exists for release %s", path.name, self.version_tag
            )
            return
        try:
            self.gh_release.upload_asset(str(path))
        except GithubException as e:
            if e.data["message"] == "Validation Failed" and [
                True
                for err in e.data["errors"]
                if err["code"] == "already_exists"  # type: ignore
            ]:
                self.logger.info(
                    "Asset %s already exists in release %s", path.name, self.version_tag
                )

    def mark_finished(self, status: STATUS) -> None:
        self.logger.info("Mark the release as finished with status '%s'", status)
        # ???: The failed release shouldn't create the mark preventing restart
        if status == SUCCESS:
            finished = self.release_dir / "finished"
            finished.touch()
        self.logger.info("Upload log file to S3")
        key = p.join(
            self.release_branch, self.commit.sha, "release", self._log_file.name
        )
        metadata = {"ContentType": "text/plain; charset=utf-8"}
        self.ch.s3_client.upload_file(
            self._log_file,
            self.ch.s3_test_reports_bucket,
            key,
            ExtraArgs=metadata,
        )
        if status == SUCCESS:
            description = "Release artifacts successfully deployed"
        elif status == FAILURE:
            description = "Failed to deploy release artifacts"
        log_url = p.join(self.ch.s3_test_reports_url, key)
        self.commit.create_status(
            status,
            log_url,
            description,
            "Release deployment",
        )

    @staticmethod
    def is_processed(version_tag: str) -> bool:
        releases_dir = get_releases_dir()
        return (releases_dir / version_tag / "finished").exists()

    @staticmethod
    def log_file(version_tag: str) -> Path:
        release_dir = get_releases_dir() / version_tag
        # To avoid chicken <-> egg issue, if somebody requested the file, we
        # create the directory unless it exists
        release_dir.mkdir(0o750, parents=True, exist_ok=True)
        return release_dir / Release.LOG_NAME

    @property
    def tag(self) -> GitTag:
        if self._tag is None:
            try:
                # The first tag is received as ref/tags/version_tag, and is highly
                # likely annotated tag. That's why we need to get the tag from ref.sha
                ref = self.ch.gh_repo.get_git_ref(f"tags/{self.version_tag}")
                self._tag = self.ch.gh_repo.get_git_tag(ref.object.sha)
            except UnknownObjectException as exc:
                raise ReleaseException(
                    f"Tag for version_tag '{self.version_tag}' is not found"
                ) from exc

        return self._tag

    @property
    def commit(self) -> Commit:
        if self._commit is None:
            try:
                self._commit = self.ch.gh_repo.get_commit(self.tag.object.sha)
            except UnknownObjectException as exc:
                raise ReleaseException(
                    f"Tag for version_tag '{self.version_tag}' is not found"
                ) from exc

        return self._commit

    @property
    def gh_release(self) -> GitRelease:
        if self._gh_release is None:
            try:
                self._gh_release = self.ch.gh_repo.get_release(self.version_tag)
            except UnknownObjectException as exc:
                raise ReleaseException(
                    f"Release for version_tag '{self.version_tag}' is not found"
                ) from exc

        return self._gh_release

    @property
    def assets(self) -> List[GitReleaseAsset]:
        """doc"""
        if not self._assets:
            self._assets = list(self.gh_release.get_assets())
        return self._assets

    @property
    def packages(self) -> Packages:
        """downloaded packages for the release"""
        if self._packages is None:
            self._packages = Packages(
                self.release_dir, self.builds_prefix, self.version
            )

        return self._packages

    @packages.setter
    def packages(self, packages: Packages) -> None:
        self._packages = packages

    @property
    def additional_version_types(self) -> List[str]:
        if self.version_type == "lts":
            return ["stable"]
        return []
