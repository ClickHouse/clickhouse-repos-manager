#!/usr/bin/env python
from pathlib import Path
from shutil import copy2
from typing import Dict, List

import subprocess

from _vendor.shell_runner import Runner
from context_helper import DEB_REPO_TEMPLATE, DebParam
from jinja2 import Template
from packages import Packages, Package

runner = Runner()


def check_dir_exist_or_create(path: Path) -> None:
    if not path.exists():
        path.mkdir(parents=True)
    if not path.is_dir():
        raise RepoException(f"the file {path} must be a directory")


class RepoException(BaseException):
    pass


class DebRepo:
    # Check requirements
    runner("reprepro --version", stderr=subprocess.STDOUT)
    _reprepro_config = Path("configs") / "deb"
    dists_config = _reprepro_config / "conf" / "distributions"

    def __init__(
        self, packages: List[Package], repo_root: Path, repo_config: Dict[str, DebParam]
    ):
        non_deb_pkgs = [pkg for pkg in packages if not pkg.path.name.endswith(".deb")]
        if non_deb_pkgs:
            raise RepoException(f"all packages must end with '.deb': {non_deb_pkgs}")

        self.packages = packages
        self._repo_config = repo_config
        self._repo_root = repo_root
        self.check_dirs()

    def add_packages(self, version_type: str, *additional_version_types: str):
        deb_files = " ".join(pkg.path.as_posix() for pkg in self.packages)
        command = (
            f"reprepro --basedir '{self.reprepro_config}' --verbose --outdir "
            f"'{self.outdir_path}' includedeb '{version_type}' {deb_files}"
        )
        runner(command)
        for additional_version_type in additional_version_types:
            self.process_additional_packages(version_type, additional_version_type)

    def process_additional_packages(
        self, original_version_type: str, additional_version_type: str
    ):
        packages_with_versions = " ".join(
            f"{pkg.path}={pkg.version}" for pkg in self.packages
        )
        command = (
            f"reprepro --basedir '{self.reprepro_config}' --verbose --outdir "
            f"'{self.outdir_path}' copy {additional_version_type} "
            f"{original_version_type} {packages_with_versions}"
        )
        runner(command)

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


class RpmRepo:
    # Check requirements
    runner("createrepo_c --version")
    runner("gpg --version")

    def __init__(self, packages: List[Package], repo_root: Path, signing_key: str):
        non_rpm_pkgs = [pkg for pkg in packages if not pkg.path.name.endswith(".rpm")]
        if non_rpm_pkgs:
            raise RepoException(f"all packages must end with '.rpm': {non_rpm_pkgs}")

        self.packages = packages
        self._repo_root = repo_root
        self.signing_key = signing_key
        check_dir_exist_or_create(self.outdir_path)

    def add_packages(self, version_type: str, *additional_version_types: str):
        dest_dir = self.outdir_path / version_type
        check_dir_exist_or_create(dest_dir)

        for package in self.packages:
            copy2(package.path, self.outdir_path / version_type)

        commands = (
            f"createrepo_c --local-sqlite --workers=2 --update --verbose {dest_dir}",
            f"gpg --sign-with {self.signing_key} --detach-sign --batch --yes "
            f"--armor {dest_dir / 'repodata' / 'repomd.xml'}",
        )
        for command in commands:
            runner(command)

        update_public_key = f"gpg --armor --export {self.signing_key}"
        pub_key_path = dest_dir / "repodata" / "repomd.xml.key"
        pub_key_path.write_text(runner(update_public_key))

        for additional_version_type in additional_version_types:
            self.add_packages(additional_version_type)

    @property
    def outdir_path(self) -> Path:
        return self._repo_root / "rpm"


class TgzRepo:
    def __init__(self, packages: List[Package], repo_root: Path):
        non_tgz_pkgs = [pkg for pkg in packages if not pkg.path.name.endswith(".tgz")]
        if non_tgz_pkgs:
            raise RepoException(f"all packages must end with '.tgz': {non_tgz_pkgs}")
        self.packages = packages
        self._repo_root = repo_root
        check_dir_exist_or_create(self.outdir_path)

    def add_packages(self, version_type: str, *additional_version_types: str):
        dest_dir = self.outdir_path / version_type
        check_dir_exist_or_create(dest_dir)

        for package in self.packages:
            copy2(package.path, self.outdir_path / version_type)

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
        self.deb = DebRepo(packages.deb, self.root, deb_config)
        self.rpm = RpmRepo(packages.rpm, self.root, signing_key)
        self.tgz = TgzRepo(packages.tgz, self.root)

    def add_packages(self):
        self.deb.add_packages(self.version_type, *self.additional_version_types)
        self.rpm.add_packages(self.version_type, *self.additional_version_types)
        self.tgz.add_packages(self.version_type, *self.additional_version_types)
