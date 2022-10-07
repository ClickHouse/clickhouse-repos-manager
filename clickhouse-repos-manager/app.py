#!/usr/bin/env python

from pathlib import Path
from typing import Optional

from _vendor.get_robot_token import get_best_robot_token
from args_helper import to_bool
from context_helper import get_working_dir, get_releases_dir
from flask import Flask, jsonify, request
from release import Release, ReleaseException


app = Flask(__name__)
app.config["REPOSITORY"] = "ClickHouse/ClickHouse"
app.config["WORKING_DIR"] = Path.home() / "clickhouse-repository-manager"
app.config["S3_BUILDS_BUCKET"] = "clickhouse-builds"
app.config["S3_TEST_REPORTS_BUCKET"] = "clickhouse-test-reports"
app.config["S3_URL"] = "https://s3.amazonaws.com"
# We have a predefined tree, so I prefer to stick to it here and define only the root
# The only directory that must be created in advance is configs/deb
# r2/
# ├── configs
# │   └── deb
# ├── deb
# │   ├── dists
# │   └── pool
# ├── rpm
# │   ├── lts
# │   └── stable
# └── tgz
#     ├── lts
#     └── stable
app.config["REPOS_ROOT"] = Path.home() / "r2"
app.config.from_prefixed_env("CHRM")
app.config["GITHUB_TOKEN"] = app.config.get("GITHUB_TOKEN", get_best_robot_token())


@app.before_first_request
def prepare_dirs():
    get_working_dir().mkdir(mode=0o750, parents=True, exist_ok=True)
    get_releases_dir().mkdir(mode=0o750, parents=True, exist_ok=True)


@app.route("/")
def root():
    return jsonify({"name": "alice", "email": "alice@outlook.com"})


@app.route("/release/", methods=["GET"])
@app.route("/release/<string:version>", methods=["GET"])
def get_release(version: Optional[str] = None):
    if version:
        return jsonify({"version": version})
    return jsonify(["all", "versions"])


@app.route("/release/<string:version_tag>", methods=["POST"])
def upload_release(version_tag: str):
    """
    0. Verify the tag
    1. Verify that the version exists in github as both tag and release
      https://api.github.com/repos/ClickHouse/ClickHouse/git/ref/tags/v22.8.2.11-lts
      https://api.github.com/repos/ClickHouse/ClickHouse/releases/tags/v22.8.2.11-lts
    2. Get its hash from git/ref/tags/v22.8.2.11-lts
      ** done **
    3. Get the packages for the hash from S3 via _vendor.packages.Packages
    4. Get the additional binaries for the assets, the list is passed as
       'binary' argument
    """
    if Release.is_processed(version_tag) and not request.args.get("force", False):
        return "PLACEHOLDER FOR LOGS", 200

    try:
        # We know now that release exists
        release = Release(version_tag, request.args.getlist("binary"))
    except ReleaseException as e:
        return str(e), 400
    except BaseException as e:
        return str(e), 500

    sync = request.args.get("sync", default=False, type=to_bool)

    try:
        release.do(sync)
    except Exception as e:
        return str(e), 500

    return (
        jsonify(
            tag=release.tag.tag,
            release=release.git_release.title,
            commit=release.commit.sha,
        ),
        202,
    )


if __name__ == "__main__":
    app.run()
