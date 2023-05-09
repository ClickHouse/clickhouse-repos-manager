The repository to operate packages.clickhouse.com

## Dependencies

### OS binaries
For the repository management the next binaries are used (ubuntu 22.04):

- `reprepro` version 5.4.1 built from [official git repo](https://salsa.debian.org/debian/reprepro/)
  - `sudo apt-get install build-essential:native libgpgme-dev libdb-dev libbz2-dev liblzma-dev libarchive-dev shunit2:native db-util:native devscripts`
  - `dpkg-buildpackage -b --no-sign && sudo dpkg -i ../reprepro_$(dpkg-parsechangelog --show-field Version)_$(dpkg-architecture -q DEB_HOST_ARCH).deb` in the sources directory
- `createrepo_c` from official ubuntu [repositories](https://packages.ubuntu.com/search?suite=default&section=all&arch=any&keywords=createrepo-c&searchon=names)
- `gpg` from official ubuntu [repositories](https://packages.ubuntu.com/search?suite=default&section=all&arch=any&keywords=gpg&searchon=names)

To mount the CloudFlare R2 storage, we use [geesefs](https://github.com/yandex-cloud/geesefs/releases). Here's the `/etc/fstab` mount description:

```fstab
packages /home/ubuntu/r2 fuse.geesefs _netdev,user_id=1000,group_id=1000,--cheap,--file-mode=0666,--dir-mode=0777,--endpoint=https://account_id.r2.cloudflarestorage.com,--shared-config=/home/ubuntu/.r2_auth,--memory-limit=2050,--gc-interval=100,--max-flushers=2,--max-parallel-parts=3,--max-parallel-copy=2 0 0
```

The `geesefs` binary should be in the `$PATH`, `--shared-config` has the AWS credentials format

```
[default]
aws_access_key_id = ***
aws_secret_access_key = ***
```

### Python
The project uses poetry to maintain the dependencies

### Launch
From the root of the repository:

```
poetry update
while true; do CHRM_WORKING_DIR=~/ebs/clickhouse-repos-manager poetry run uwsgi --http=[::]:5000 --chdir=clickhouse-repos-manager --module=app --callable=app --http-timeout=10800 --processes=1 --enable-threads; sleep 10; done
```

The service runs in a single-threaded single-process mode, so the request will be blocked until existing finished.

TODO: a proper systemd.service

### Config
All the configuration environment parameters are listed in `clickhouse-repos-manager/app_config.py`. Defaults work for ClickHouse/ClickHouse repository.

#### Debian repository configuration
The ENV parameters starting with `DEB_REPO_` are converted from `DEB_REPO_PARAMETER_NAME` to `ParameterName` and used in reprepro.
