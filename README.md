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

## To be done

- [ ] Logging


### `reprepro` configuration

The file used in `reprepro` is in [configs/reprepro/distributions](./configs/reprepro/distributions)

#### A commands list to implement

```
python3 push_to_artifactory.py --release 'refs/tags/v22.9.1.2603-stable' --commit '3030d4c7ff09ec44ab07d0a8069ea923227288a1' --all -n
# deb
reprepro -b ~/ebs/configs/deb/ --outdir='+b/../../deb' includedeb stable ./tmp/push_to_artifactory/clickhouse-*.deb
# rpm
cp tmp/push_to_artifactory/*.rpm ~/ebs/rpm/stable/
createrepo_c --local-sqlite --workers=3 --update --verbose ~/r2/rpm/stable/
gpg --sign-with B5487D377C749E91 --detach-sign --batch --yes --armor ~/ebs/rpm/stable/repodata/repomd.xml
# tgz
cp tmp/push_to_artifactory/clickhouse-*tgz ~/ebs/tgz/stable/

# rsync
rsync ~/ebs/{deb,rpm,tgz} ~/r2 -rPm --no-times --size-only --delete --include='*.deb' --include='*.rpm'  --include='*.tgz' --include='*/' --exclude='*'
rsync ~/ebs/{deb,rpm,tgz} ~/r2 -rPm --delete --include='**/dists/**' --include='**/repodata/*' --include='*/' --exclude='*'
rsync ~/ebs/{deb,rpm,tgz} ~/r2 -rPm --no-times --size-only --delete


python3 download_binary.py --version 'refs/tags/v22.9.1.2603-stable' --commit '3030d4c7ff09ec44ab07d0a8069ea923227288a1' binary_darwin binary_darwin_aarch64
mv tmp/download_binary/clickhouse-macos* tmp/push_to_artifactory/

gh release upload v22.9.1.2603-stable tmp/push_to_artifactory/*


# Cycle
git fetch --tags
for tag in v22.8.11.15-lts; do rm -rf tmp/ && commit=$(git rev-parse "$tag"^{}) && python3 push_to_artifactory.py --release "$tag" --commit "$commit" --all -n && reprepro -b ~/r2/configs/deb/ --outdir='+b/../../deb' --keepunusednewfiles includedeb stable ./tmp/push_to_artifactory/clickhouse-*.deb && sleep 100 && cp tmp/push_to_artifactory/*.rpm ~/r2/rpm/stable/ && sleep 100 && createrepo_c --local-sqlite --workers=3 --update --verbose ~/r2/rpm/stable/ && gpg --sign-with B5487D377C749E91 --detach-sign --batch --yes --armor ~/r2/rpm/stable/repodata/repomd.xml && sleep 20 && cp tmp/push_to_artifactory/clickhouse-*tgz ~/r2/tgz/stable/ && python3 download_binary.py --version "$tag" --commit "$commit" binary_darwin binary_darwin_aarch64 ; mv tmp/download_binary/clickhouse-macos* tmp/push_to_artifactory/; gh release upload "$tag" tmp/push_to_artifactory/*; if [[ "$tag" == *"-lts" ]]; then version=${tag#v} && version=${version/-*} && reprepro -b ~/r2/configs/deb/ --outdir='+b/../../deb' copy lts stable clickhouse-client="$version" clickhouse-common-static="$version" clickhouse-common-static-dbg="$version" clickhouse-server="$version" && sleep 20 && cp ./tmp/push_to_artifactory/clickhouse-*"$version"*.rpm ~/r2/rpm/lts/ && sleep 100 && createrepo_c --local-sqlite --workers=3 --verbose --update ~/r2/rpm/lts/ && gpg --sign-with B5487D377C749E91 --detach-sign --batch --yes --armor ~/r2/rpm/lts/repodata/repomd.xml && sleep 5 && cp ~/r2/tgz/stable/*"$version"* ~/r2/tgz/lts/; fi; done

# Helping things
# Duplicate to LTS
version=22.8.6.71
reprepro -b ~/r2/configs/deb/ --outdir='+b/../../deb' copy lts stable clickhouse-client="$version" clickhouse-common-static="$version" clickhouse-common-static-dbg="$version" clickhouse-server="$version"
cp ./tmp/push_to_artifactory/clickhouse-*"$version"*.rpm ~/r2/rpm/lts/
createrepo_c --local-sqlite --workers=3 --update --verbose ~/r2/rpm/lts/
gpg --sign-with B5487D377C749E91 --detach-sign --batch --yes --armor ~/r2/rpm/lts/repodata/repomd.xml
cp ~/r2/tgz/stable/*"$version"* ~/r2/tgz/lts/

# one line
version=22.8.9.24 && reprepro -b ~/r2/configs/deb/ --outdir='+b/../../deb' copy lts stable clickhouse-client="$version" clickhouse-common-static="$version" clickhouse-common-static-dbg="$version" clickhouse-server="$version" && sleep 20 && cp ./tmp/push_to_artifactory/clickhouse-*"$version"*.rpm ~/r2/rpm/lts/ && sleep 100 && createrepo_c --local-sqlite --workers=3 --verbose --update ~/r2/rpm/lts/ && gpg --sign-with B5487D377C749E91 --detach-sign --batch --yes --armor ~/r2/rpm/lts/repodata/repomd.xml && sleep 5 && cp ~/r2/tgz/stable/*"$version"* ~/r2/tgz/lts/
```
