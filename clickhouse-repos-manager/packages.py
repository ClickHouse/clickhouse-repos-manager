from collections import namedtuple
from os import path as p
from pathlib import Path
from typing import Iterable, Iterator, List
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
    optional_packages = ("clickhouse-keeper-config",)

    def __init__(self, path: Path, url_prefix: str, version: str):
        self.url_prefix = url_prefix
        # Dicts of name: s3_path_suffix
        self.deb = []  # type: List[Package]
        self.rpm = []  # type: List[Package]
        self.tgz = []  # type: List[Package]
        self.tgz_sha = []  # type: List[Package]
        self.optional_deb = []  # type: List[Package]
        self.optional_rpm = []  # type: List[Package]
        self.optional_tgz = []  # type: List[Package]
        self.optional_tgz_sha = []  # type: List[Package]
        for check in self.checks:

            def deb(name: str) -> Path:
                return path / f"{name}_{version}_{check.deb_arch}.deb"

            def rpm(name: str) -> Path:
                return path / f"{name}-{version}.{check.rpm_arch}.rpm"

            def tgz(name: str, with_sha: bool) -> Path:
                sha = ".sha512" if with_sha else ""
                return path / f"{name}-{version}-{check.deb_arch}.tgz{sha}"

            for name in self.packages:
                self.deb.append(Package(check.check_name, deb(name), version))
                self.rpm.append(Package(check.check_name, rpm(name), version))
                self.tgz.append(Package(check.check_name, tgz(name, False), version))
                self.tgz_sha.append(Package(check.check_name, tgz(name, True), version))

            for name in self.optional_packages:
                self.optional_deb.append(Package(check.check_name, deb(name), version))
                self.optional_rpm.append(Package(check.check_name, rpm(name), version))
                self.optional_tgz.append(
                    Package(check.check_name, tgz(name, False), version)
                )
                self.optional_tgz_sha.append(
                    Package(check.check_name, tgz(name, True), version)
                )

    def download(
        self, overwrite: bool, *packages: Iterable[str], logger: logging.Logger = logger
    ) -> None:
        def log_download(pkg_type: str, pkgs: List[Package]) -> None:
            logger.info(
                "Downloading %s packages:\n  %s",
                pkg_type,
                "\n  ".join(p.path.name for p in pkgs),
            )

        def helper(pkg_type: str, pkgs: List[Package], opt_pkgs: List[Package]) -> None:
            log_download(pkg_type, pkgs)
            for pkg in pkgs:
                pkg.download(self.url_prefix, overwrite, logger)
            if opt_pkgs:
                log_download(f"optional {pkg_type}", opt_pkgs)
                for pkg in opt_pkgs:
                    try:
                        pkg.download(self.url_prefix, overwrite, logger)
                        pkgs.append(pkg)
                    except:
                        logger.warning(
                            "Failed to download optional package %s, continue",
                            pkg.name,
                        )

        if not packages:
            packages = ("deb", "rpm", "tgz")

        if "deb" in packages:
            helper("deb", self.deb, self.optional_deb)
        if "rpm" in packages:
            helper("rpm", self.rpm, self.optional_rpm)
        if "tgz" in packages:
            helper("tgz", self.tgz, self.optional_tgz)
            helper("tgz_sha", self.tgz_sha, self.optional_tgz_sha)

    def all(self) -> Iterator[Package]:
        for packages in (self.deb, self.rpm, self.tgz, self.tgz_sha):
            for package in packages:
                yield package
