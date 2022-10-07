#!/usr/bin/env python
from pathlib import Path
from shutil import copy
from typing import List

from _vendor.packages import Packages, Package


class RepoException(BaseException):
    pass


class DebRepo:
    def __init__(self, packages: List[Package], repo_root: Path):
        non_deb_pkgs = [pkg for pkg in packages if not pkg.path.name.endswith(".deb")]
        if non_deb_pkgs:
            raise RepoException(f"all packages must end with '.deb': {non_deb_pkgs}")
        self.packages = packages
        self._repo_root = repo_root
        self.check_dirs()

    def check_dirs(self):
        if not self.config_path.is_dir():
            raise RepoException(f"{self.config_path} must exist")

        if not self.outdir_path.exists():
            self.outdir_path.mkdir(parents=True)
        if not self.outdir_path.is_dir():
            raise RepoException(f"{self.outdir_path} must exist")

    @property
    def config_path(self) -> Path:
        return self._repo_root / "configs" / "deb"

    @property
    def outdir_path(self) -> Path:
        return self._repo_root / "deb"


class RpmRepo:
    def __init__(self, packages: List[Package], repo_root: Path):
        non_rpm_pkgs = [pkg for pkg in packages if not pkg.path.name.endswith(".rpm")]
        if non_rpm_pkgs:
            raise RepoException(f"all packages must end with '.rpm': {non_rpm_pkgs}")
        self.packages = packages
        self._repo_root = repo_root
        self.check_dirs()

    def add_packages(self, version_type: str, *additional_version_types: str):
        dest_dir = self.outdir_path / version_type
        if not dest_dir.exists():
            dest_dir.mkdir(parents=True)
        if not dest_dir.is_dir():
            raise RepoException(f"Destination dir {dest_dir} must be a directory")

        for package in self.packages:
            copy(package.path, self.outdir_path / version_type)

        # createrepo_c --local-sqlite --workers=2 --update {dest_dir}
        # gpg --sign-with B5487D377C749E91 --detach-sign --batch --yes --armor \
        #     {dest_dir / "repodata" / "repomd.xml"}

        for additional_version_type in additional_version_types:
            self.add_packages(additional_version_type)

    def check_dirs(self):
        if not self.outdir_path.exists():
            self.outdir_path.mkdir(parents=True)
        if not self.outdir_path.is_dir():
            raise RepoException(f"{self.outdir_path} must exist")

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
        self.check_dirs()

    def add_packages(self, version_type: str, *additional_version_types: str):
        dest_dir = self.outdir_path / version_type
        if not dest_dir.exists():
            dest_dir.mkdir(parents=True)
        if not dest_dir.is_dir():
            raise RepoException(f"Destination dir {dest_dir} must be a directory")

        for package in self.packages:
            copy(package.path, self.outdir_path / version_type)

        for additional_version_type in additional_version_types:
            self.add_packages(additional_version_type)

    def check_dirs(self):
        if not self.outdir_path.exists():
            self.outdir_path.mkdir(parents=True)
        if not self.outdir_path.is_dir():
            raise RepoException(f"{self.outdir_path} must exist")

    @property
    def outdir_path(self) -> Path:
        return self._repo_root / "tgz"


class Repos:
    def __init__(
        self,
        packages: Packages,
        repo_root: Path,
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
        self.deb = DebRepo(packages.deb, self.root)
        self.rpm = RpmRepo(packages.rpm, self.root)
        self.tgz = TgzRepo(packages.tgz, self.root)

    def add_packages(self):
        # self.deb.add_packages(self.version_type, *self.additional_version_types)
        self.rpm.add_packages(self.version_type, *self.additional_version_types)
        self.tgz.add_packages(self.version_type, *self.additional_version_types)
