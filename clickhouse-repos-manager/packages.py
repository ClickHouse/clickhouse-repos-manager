from collections import namedtuple
from os import path as p
from pathlib import Path
from typing import Iterator, List
import logging

from _vendor.download_helper import download

CheckArch = namedtuple("CheckArch", ("check_name", "deb_arch", "rpm_arch"))

logger = logging.getLogger(__name__)


class Package:
    def __init__(self, check_name: Path, path: Path, version: str):
        self.path = path
        self.s3_suffix = check_name / path.name
        self._version = version

    @property
    def name(self) -> str:
        return self.path.name

    @property
    def exists(self) -> bool:
        return self.path.is_file()

    @property
    def version(self) -> str:
        return self._version

    def download(
        self, url_prefix: str, overwrite: bool = False, logger: logging.Logger = logger
    ) -> None:
        if not overwrite and self.path.exists():
            logger.info("File %s already exists, skipping", self.path)
            return

        try:
            # join doesn't remove double slash from the url_prefix,
            # that's why it's used instead of Path
            download(p.join(url_prefix, self.s3_suffix), self.path, logger=logger)
        except (BaseException, Exception) as e:
            logger.error("Failed to download package %s, removing", self.name)
            self.path.unlink(True)
            logger.error("Exception: %s", e.with_traceback)
            raise


class Packages:
    checks = (
        CheckArch(Path("package_release"), "amd64", "x86_64"),
        CheckArch(Path("package_aarch64"), "arm64", "aarch64"),
    )
    packages = (
        "clickhouse-client",
        "clickhouse-common-static",
        "clickhouse-common-static-dbg",
        "clickhouse-keeper",
        "clickhouse-keeper-dbg",
        "clickhouse-server",
    )

    def __init__(self, path: Path, url_prefix: str, version: str):
        self.url_prefix = url_prefix
        # Dicts of name: s3_path_suffix
        self.deb = []  # type: List[Package]
        self.rpm = []  # type: List[Package]
        self.tgz = []  # type: List[Package]
        self.tgz_sha = []  # type: List[Package]
        for check in self.checks:
            for name in self.packages:
                deb = path / f"{name}_{version}_{check.deb_arch}.deb"
                self.deb.append(Package(check.check_name, deb, version))

                rpm = path / f"{name}-{version}.{check.rpm_arch}.rpm"
                self.rpm.append(Package(check.check_name, rpm, version))

                tgz = path / f"{name}-{version}-{check.deb_arch}.tgz"
                self.tgz.append(Package(check.check_name, tgz, version))
                tgz_sha = path / f"{name}-{version}-{check.deb_arch}.tgz.sha512"
                self.tgz_sha.append(Package(check.check_name, tgz_sha, version))

    def download(
        self, overwrite: bool, *packages: Iterable[str], logger: logging.Logger = logger
    ) -> None:
        if not packages:
            packages = ("deb", "rpm", "tgz")

        def log_download(pkg_type: str, pkgs: List[Package]) -> None:
            logger.info(
                "Downloading %s packages:\n  %s",
                pkg_type,
                "\n  ".join(p.path.name for p in pkgs),
            )

        # Boilerplating to have a proper type hinting
        if "deb" in packages:
            log_download("deb", self.deb)
            for package in self.deb:
                package.download(self.url_prefix, overwrite, logger)
        if "rpm" in packages:
            log_download("rpm", self.rpm)
            for package in self.rpm:
                package.download(self.url_prefix, overwrite, logger)
        if "tgz" in packages:
            log_download("tgz", self.tgz)
            for package in self.tgz:
                package.download(self.url_prefix, overwrite, logger)
            log_download("tgz_sha", self.tgz_sha)
            for package in self.tgz_sha:
                package.download(self.url_prefix, overwrite, logger)

    def all(self) -> Iterator[Package]:
        for packages in (self.deb, self.rpm, self.tgz, self.tgz_sha):
            for package in packages:
                yield package
