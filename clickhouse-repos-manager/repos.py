#!/usr/bin/env python
from logging import getLogger, Logger
from pathlib import Path
from shutil import copy2
from typing import Dict, List, Union

import subprocess

from jinja2 import Template

from _vendor.shell_runner import Runner
from app_config import DEB_REPO_TEMPLATE
from context_helper import DebParam
from packages import Packages, Package

runner = Runner()

logger = getLogger(__name__)


def check_dir_exist_or_create(path: Path) -> None:
    if not path.exists():
        path.mkdir(parents=True)
    if not path.is_dir():
        raise RepoException(f"the file {path} must be a directory")


def copy_if_not_exists(src: Path, dst: Path) -> Union[Path, str]:
    if dst.is_dir():
        dst = dst / src.name
    if not dst.exists():
        return copy2(src, dst)
    if src.stat().st_size == dst.stat().st_size:
        return dst
    return copy2(src, dst)


class RepoException(BaseException):
    pass


class DebRepo:
    # Check requirements
    runner("reprepro --version", stderr=subprocess.STDOUT)
    _reprepro_config = Path("configs") / "deb"
    dists_config = _reprepro_config / "conf" / "distributions"

    def __init__(
        self,
        packages: List[Package],
        repo_root: Path,
        repo_config: Dict[str, DebParam],
        logger: Logger = logger,
    ):
        non_deb_pkgs = [pkg for pkg in packages if not pkg.path.name.endswith(".deb")]
        if non_deb_pkgs:
            raise RepoException(f"all packages must end with '.deb': {non_deb_pkgs}")

        self.packages = packages
        self._repo_config = repo_config
        self._repo_root = repo_root
        self.logger = logger
        self.check_dirs()

    def add_packages(self, version_type: str, *additional_version_types: str):
        deb_files = " ".join(pkg.path.as_posix() for pkg in self.packages)
        command = f"{self.reprepro_cmd} includedeb '{version_type}' {deb_files}"
        self.logger.info("Deploying DEB packages to codename %s", version_type)
        self.logger.info(
            "Deployment logs:\n%s", runner(command, stderr=subprocess.STDOUT)
        )
        for additional_version_type in additional_version_types:
            self.process_additional_packages(version_type, additional_version_type)

    def process_additional_packages(
        self, original_version_type: str, additional_version_type: str
    ):
        packages_with_versions = " ".join(
            f"{pkg.path}={pkg.version}" for pkg in self.packages
        )
        command = (
            f"{self.reprepro_cmd} copy {additional_version_type} "
            f"{original_version_type} {packages_with_versions}"
        )
        self.logger.info(
            "Deploying DEB packages to additional codename %s", additional_version_type
        )
        self.logger.info(
            "Deployment logs:\n%s", runner(command, stderr=subprocess.STDOUT)
        )

    def check_dirs(self):
        dists_config = self._repo_root / self.dists_config
        check_dir_exist_or_create(dists_config.parent)
        if not dists_config.exists():
            tmpl = Template(DEB_REPO_TEMPLATE)
            dists_config.write_text(tmpl.render(conf=self._repo_config))

        check_dir_exist_or_create(self.outdir_path)

    @property
    def outdir_path(self) -> Path:
        return self._repo_root / "deb"

    @property
    def reprepro_config(self) -> Path:
        return self._repo_root / DebRepo._reprepro_config

    @property
    def reprepro_cmd(self) -> str:
        return (
            f"reprepro --basedir '{self.reprepro_config}' --verbose --export=force "
            f"--outdir '{self.outdir_path}'"
        )


class RpmRepo:
    # Check requirements
    runner("createrepo_c --version")
    runner("gpg --version")

    def __init__(
        self,
        packages: List[Package],
        repo_root: Path,
        signing_key: str,
        logger: Logger = logger,
    ):
        non_rpm_pkgs = [pkg for pkg in packages if not pkg.path.name.endswith(".rpm")]
        if non_rpm_pkgs:
            raise RepoException(f"all packages must end with '.rpm': {non_rpm_pkgs}")

        self.packages = packages
        self._repo_root = repo_root
        self.signing_key = signing_key
        self.logger = logger
        check_dir_exist_or_create(self.outdir_path)

    def add_packages(self, version_type: str, *additional_version_types: str):
        dest_dir = self.outdir_path / version_type
        check_dir_exist_or_create(dest_dir)

        self.logger.info("Copying RPM packages to %s directory", version_type)
        for package in self.packages:
            copy_if_not_exists(package.path, self.outdir_path / version_type)

        commands = (
            f"createrepo_c --local-sqlite --workers=2 --update --verbose {dest_dir}",
            f"gpg --sign-with {self.signing_key} --detach-sign --batch --yes --armor "
            f"{dest_dir / 'repodata' / 'repomd.xml'}",
        )
        self.logger.info("Updating index for RPM packages in %s", version_type)
        for command in commands:
            self.logger.info(
                "Output for command %s:\n%s",
                command.split(maxsplit=1)[0],
                runner(command, stderr=subprocess.STDOUT),
            )

        update_public_key = f"gpg --armor --export {self.signing_key}"
        pub_key_path = dest_dir / "repodata" / "repomd.xml.key"
        self.logger.info("Updating repomd.xml.key")
        pub_key_path.write_text(runner(update_public_key))

        for additional_version_type in additional_version_types:
            self.add_packages(additional_version_type)

    @property
    def outdir_path(self) -> Path:
        return self._repo_root / "rpm"


class TgzRepo:
    def __init__(
        self, packages: List[Package], repo_root: Path, logger: Logger = logger
    ):
        non_tgz_pkgs = [
            pkg
            for pkg in packages
            if not (
                pkg.path.name.endswith(".tgz") or pkg.path.name.endswith(".tgz.sha512")
            )
        ]
        if non_tgz_pkgs:
            raise RepoException(f"all packages must end with '.tgz': {non_tgz_pkgs}")
        self.packages = packages
        self._repo_root = repo_root
        self.logger = logger
        check_dir_exist_or_create(self.outdir_path)

    def add_packages(self, version_type: str, *additional_version_types: str):
        dest_dir = self.outdir_path / version_type
        check_dir_exist_or_create(dest_dir)
        self.logger.info("Deploying TGZ packages to %s", version_type)

        for package in self.packages:
            copy_if_not_exists(package.path, self.outdir_path / version_type)

        for additional_version_type in additional_version_types:
            self.add_packages(additional_version_type)

    @property
    def outdir_path(self) -> Path:
        return self._repo_root / "tgz"


class Repos:
    def __init__(
        self,
        packages: Packages,
        repo_root: Path,
        deb_config: Dict[str, DebParam],
        signing_key: str,
        version_type: str,
        *additional_version_types: str,
        logger: Logger = logger,
    ):
        """
        The class represents three different repositories:
            - debian
            - rpm
            - tgz

        Each repo is operated differently, all know what to do by when with_stable=True
        """
        if not repo_root.is_dir():
            raise RepoException(f"{repo_root} directory must exist")
        self.root = repo_root
        self.version_type = version_type
        self.additional_version_types = list(additional_version_types)
        self.logger = logger

        try:
            self.deb = DebRepo(packages.deb, self.root, deb_config, self.logger)
            self.rpm = RpmRepo(packages.rpm, self.root, signing_key, self.logger)
            tgz_packages = packages.tgz + packages.tgz_sha
            self.tgz = TgzRepo(tgz_packages, self.root, self.logger)
        except Exception as e:
            self.logger.exception(
                "Fail to prepare repositories, exception occure: %s", e
            )
            raise RepoException from e

    def add_packages(self):
        self.deb.add_packages(self.version_type, *self.additional_version_types)
        self.rpm.add_packages(self.version_type, *self.additional_version_types)
        self.tgz.add_packages(self.version_type, *self.additional_version_types)
