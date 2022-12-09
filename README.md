The repository to operate packages.clickhouse.com

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
