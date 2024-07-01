#!/usr/bin/env python

""""""

import logging
import sys
import time
from pathlib import Path
from typing import Any

import requests  # type: ignore

DOWNLOAD_RETRIES_COUNT = 3

# Used by default if not set for functions
logger = logging.getLogger(__name__)


class DownloadException(BaseException):
    pass


### Slightly patched tests/ci/build_download_helper.py


def get_with_retries(
    url: str,
    retries: int = DOWNLOAD_RETRIES_COUNT,
    sleep: int = 3,
    logger: logging.Logger = logger,
    **kwargs: Any,
) -> requests.Response:
    logger.info(
        "Getting URL with %i tries and sleep %i in between: %s", retries, sleep, url
    )
    exc = Exception("A placeholder to satisfy typing and avoid nesting")
    timeout = kwargs.pop("timeout", 30)
    for i in range(retries):
        try:
            response = requests.get(url, timeout=timeout, **kwargs)
            response.raise_for_status()
            return response
        except Exception as e:
            if i + 1 < retries:
                logger.info("Exception '%s' while getting, retry %i", e, i + 1)
                time.sleep(sleep)

            exc = e

    raise exc


def download(url: str, path: Path, logger: logging.Logger = logger) -> None:
    logger.info("Downloading from %s to temp path %s", url, path)
    for i in range(DOWNLOAD_RETRIES_COUNT):
        try:
            response = get_with_retries(url, retries=1, stream=True)
            total_length = int(response.headers.get("content-length", 0))
            if path.is_file() and total_length and path.stat().st_size == total_length:
                logger.info(
                    "The file %s already exists and have a proper size %s",
                    path,
                    total_length,
                )
                return

            with open(path, "wb") as f:
                if total_length == 0:
                    logger.info(
                        "No content-length, will download file without progress"
                    )
                    f.write(response.content)
                else:
                    dl = 0

                    logger.info("Content length is %ld bytes", total_length)
                    for data in response.iter_content(chunk_size=4096):
                        dl += len(data)
                        f.write(data)
                        if sys.stdout.isatty():
                            done = int(50 * dl / total_length)
                            percent = int(100 * float(dl) / total_length)
                            eq_str = "=" * done
                            space_str = " " * (50 - done)
                            sys.stdout.write(f"\r[{eq_str}{space_str}] {percent}%")
                            sys.stdout.flush()
            break
        except Exception as e:
            if sys.stdout.isatty():
                sys.stdout.write("\n")
            if path.exists():
                path.unlink()

            if i + 1 < DOWNLOAD_RETRIES_COUNT:
                time.sleep(3)
            else:
                raise DownloadException(
                    f"Cannot download dataset from {url}, all retries exceeded"
                ) from e

    if sys.stdout.isatty():
        sys.stdout.write("\n")
    logger.info("Downloading finished")
