#!/usr/bin/env python3

import logging
from logging.handlers import QueueHandler
from queue import Queue
from threading import Thread
from time import sleep
from typing import Optional

from flask import Flask, Response, jsonify, request, send_file

from app_config import set_config
from args_helper import to_bool
from context_helper import get_repos_root_dir, get_releases_dir
from release import Release, ReleaseException
from repos import DebRepo


app = Flask(__name__)
# We have a predefined tree, so I prefer to stick to it here and define only the root
# If the directory does not exist, everything will be created
# The deb config is created from DEB_REPO_* parameters
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

# The ENVs with "CHRM_" are processed
set_config(app)


@app.before_first_request
def prepare_dirs():
    get_releases_dir().mkdir(mode=0o750, parents=True, exist_ok=True)
    get_repos_root_dir().mkdir(mode=0o750, parents=True, exist_ok=True)
    if app.config.get("DEB_FORCE_RECONFIGURE_REPO", False):
        (get_repos_root_dir() / DebRepo.dists_config).unlink(missing_ok=True)


@app.route("/")
def root():
    return jsonify({"name": "alice", "email": "alice@outlook.com"})


@app.route("/release/", methods=["GET"])
@app.route("/release/<string:version>", methods=["GET"])
def get_release(version: Optional[str] = None):
    if version:
        return jsonify({"version": version})
    return jsonify(["all", "versions"])


@app.route("/test-logs")
def get_logs():
    from flask import Response, stream_with_context
    from logging import getLogger, INFO, Formatter
    from logging.handlers import QueueHandler
    from queue import Queue
    from io import StringIO

    buffer = Queue()
    logger = getLogger("test-logger")
    handler = QueueHandler(buffer)
    handler.setFormatter(Formatter("%(asctime)s - %(levelname)s - %(message)s\n"))
    logger.addHandler(handler)
    logger.setLevel(INFO)

    def back_log():
        for i in range(10):
            logger.info("line %s", i)
            sleep(0.8)

    thread = Thread(target=back_log)
    thread.start()

    def g():
        # to fix a potential issue
        while thread.is_alive():
            if not buffer.empty():
                yield buffer.get().getMessage()

    return Response(g(), mimetype="text/plain")


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
    log_file = Release.log_file(version_tag)
    if Release.is_processed(version_tag) and not request.args.get("force", False):
        return send_file(log_file)

    logger = logging.getLogger(f"release-{version_tag}")
    logger.setLevel(logging.INFO)
    formatter = logging.Formatter("%(asctime)s - %(levelname)s - %(message)s\n")

    # Create a handler for returning text to the response body
    sync = request.args.get("sync", default=False, type=to_bool)
    if sync:
        log_queue = Queue()  # type: Queue[logging.LogRecord]
        queue_handler = QueueHandler(log_queue)
        queue_handler.setFormatter(formatter)
        logger.addHandler(queue_handler)
        status = 200

        def generate_response(thread: Optional[Thread]):
            if thread is None:
                raise TypeError("thread must be a Thread")
            sleep(0.2)
            # to fix a potential issue
            while thread.is_alive():
                if not log_queue.empty():
                    yield log_queue.get().getMessage()

            if not release.exceptions.empty():
                raise release.exceptions.get()

    else:
        status = 202

        def generate_response(thread: Optional[Thread]):
            _ = thread
            return jsonify(
                tag=release.tag.tag,
                release=release.git_release.title,
                commit=release.commit.sha,
            )

    file_handler = logging.FileHandler(log_file, "w")
    file_handler.setFormatter(formatter)
    logger.addHandler(file_handler)

    try:
        # We know now that release exists
        release = Release(version_tag, logger, request.args.getlist("binary"))
    except ReleaseException as e:
        return str(e), 400
    except BaseException as e:
        return str(e), 500

    try:
        thread = release.do(False)
    except Exception as e:
        logger.error(
            "Exception occured during the release process: %s", e.with_traceback
        )
        return str(e.with_traceback), 500

    return Response(generate_response(thread), status=status)


if __name__ == "__main__":
    with app.app_context():
        prepare_dirs()
    app.run()
