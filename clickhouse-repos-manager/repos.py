#!/usr/bin/env python
import subprocess
import tarfile
from contextlib import contextmanager
from datetime import datetime
from logging import Logger, getLogger
from pathlib import Path
from shutil import copy2, copytree, rmtree
from tempfile import mkdtemp
from typing import Dict, Iterator, List

from jinja2 import Template

from _vendor.shell_runner import Runner
from app_config import DEB_REPO_TEMPLATE
from context_helper import DebParam
from packages import Package, Packages

runner = Runner()

logger = getLogger(__name__)


def check_dir_exist_or_create(path: Path) -> None:
    if not path.exists():
        path.mkdir(parents=True)
    if not path.is_dir():
        raise RepoException(f"the file {path} must be a directory")


def copy_if_not_exists(src: Path, dst: Path) -> Path:
    if dst.is_dir():
        dst = dst / src.name
    if not dst.exists():
        return copy2(src, dst)  # type: ignore
    if src.stat().st_size == dst.stat().st_size:
        return dst
    return copy2(src, dst)  # type: ignore


class RepoException(BaseException):
    pass


class DebRepo:
    # Check requirements
    runner("reprepro --version", stderr=subprocess.STDOUT)
    _reprepro_config = Path("configs") / "deb"
    _configs_archive = Path("configs") / "archive"
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
        self._configs_root = repo_root
        self.logger = logger
        self.check_dirs()

    def add_packages(self, version_type: str, *additional_version_types: str) -> None:
        with self.local_configs():
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
    ) -> None:
        # pair of package-name=version
        packages_with_versions = " ".join(
            set(
                f"{pkg.path.name.split('_', 1)[0]}={pkg.version}"
                for pkg in self.packages
            )
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

    def check_dirs(self) -> None:
        dists_config = self._repo_root / self.dists_config
        check_dir_exist_or_create(dists_config.parent)
        if not dists_config.exists():
            tmpl = Template(DEB_REPO_TEMPLATE)
            dists_config.write_text(tmpl.render(conf=self._repo_config))

        check_dir_exist_or_create(self.outdir_path)

    @contextmanager
    def local_configs(self) -> Iterator[None]:
        temp_dir = Path(mkdtemp())

        preserved_configs_root = self._configs_root
        original_configs = self.reprepro_config
        self._configs_root = temp_dir
        configs_copy = self.reprepro_config
        check_dir_exist_or_create(configs_copy.parent)

        self.logger.info(
            "Copy content of %s to %s for local indexing",
            original_configs,
            configs_copy,
        )
        copytree(original_configs, configs_copy)

        try:
            # copy configs
            yield
        except (Exception, BaseException) as e:
            # by any issue, restore the copied
            self.logger.error(
                "Error occured during the packages deployment, "
                "do not copy changed configs back: %s",
                e,
            )
            self._configs_root = preserved_configs_root
            rmtree(temp_dir)
            raise
        # Create an archive from previous configs' state
        now = datetime.now()
        archive_dir = self._repo_root / self._configs_archive
        check_dir_exist_or_create(archive_dir)
        with tarfile.open(archive_dir / f"deb-{now.isoformat()}.tar.gz", "w:gz") as tf:
            tf.add(original_configs, arcname="deb")
        config_archives = list(archive_dir.glob("deb-*.tar.gz"))
        # archive existing as backup, preserve last 30
        if len(config_archives) >= 30:
            for archive in config_archives[: len(config_archives) - 29]:
                archive.unlink()
        # copying updated back as the current DB
        rmtree(original_configs)
        copytree(configs_copy, original_configs)
        self._configs_root = preserved_configs_root
        rmtree(temp_dir)

    @property
    def outdir_path(self) -> Path:
        return self._repo_root / "deb"

    @property
    def reprepro_config(self) -> Path:
        return self._configs_root / DebRepo._reprepro_config

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

    def add_packages(self, version_type: str, *additional_version_types: str) -> None:
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

    def add_packages(self, version_type: str, *additional_version_types: str) -> None:
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
        self.logger = logger
        self._remount_root()
        self.version_type = version_type
        self.additional_version_types = list(additional_version_types)

        try:
            self.deb = DebRepo(packages.deb, self.root, deb_config, self.logger)
            self.rpm = RpmRepo(packages.rpm, self.root, signing_key, self.logger)
            tgz_packages = packages.tgz + packages.tgz_sha
            self.tgz = TgzRepo(tgz_packages, self.root, self.logger)
        except (BaseException, Exception) as e:
            self.logger.exception(
                "Fail to prepare repositories, exception occure: %s", e
            )
            raise RepoException("Failed to create the repositories class") from e

    def _remount_root(self) -> None:
        """regular remount should address accumulated inconsistency of geesefs+R2"""
        root = self.root.absolute()
        try:
            runner(f"findmnt -J -s {root}", stderr=subprocess.STDOUT)
        except subprocess.SubprocessError:
            self.logger.info(
                "The repositories root directory is not a mountpoint, "
                "do not re-mount it"
            )
            return

        mount_cmd = f"mount {root}"
        try:
            runner(f"mountpoint {root}", stderr=subprocess.STDOUT)
        except subprocess.SubprocessError:
            self.logger.info(
                "The repositories root directory is not mounted, just mount it"
            )
            runner(mount_cmd, stderr=subprocess.STDOUT)
            return

        # The self.root is currently mounted, remount it
        self.logger.info("Remounting repositories root directory")
        runner(f"u{mount_cmd}", stderr=subprocess.STDOUT)
        runner(mount_cmd, stderr=subprocess.STDOUT)

    def add_packages(self) -> None:
        self.deb.add_packages(self.version_type, *self.additional_version_types)
        self.rpm.add_packages(self.version_type, *self.additional_version_types)
        self.tgz.add_packages(self.version_type, *self.additional_version_types)
