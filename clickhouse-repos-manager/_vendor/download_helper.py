from pathlib import Path
from typing import Optional
import logging
import sys
import time

import requests  # type: ignore

DOWNLOAD_RETRIES_COUNT = 3


def get_with_retries(
    url: str,
    retries: int = DOWNLOAD_RETRIES_COUNT,
    sleep: int = 3,
    **kwargs,
) -> requests.Response:
    logging.info(
        "Getting URL with %i tries and sleep %i in between: %s", retries, sleep, url
    )
    exc = None  # type: Optional[Exception]
    for i in range(retries):
        try:
            response = requests.get(url, **kwargs)
            response.raise_for_status()
            break
        except Exception as e:
            if i + 1 < retries:
                logging.info("Exception '%s' while getting, retry %i", e, i + 1)
                time.sleep(sleep)

            exc = e
    else:
        raise Exception(exc)

    return response


def download_with_progress(url: str, path: Path):
    logging.info("Downloading from %s to path %s", url, path)
    path_tmp = path.with_suffix(".tmp")
    for i in range(DOWNLOAD_RETRIES_COUNT):
        try:
            with open(path_tmp, "wb") as f:
                response = get_with_retries(url, retries=1, stream=True)
                total_length = response.headers.get("content-length")
                if total_length is None or int(total_length) == 0:
                    logging.info(
                        "No content-length, will download file without progress"
                    )
                    f.write(response.content)
                else:
                    dl = 0
                    total_length = int(total_length)
                    logging.info("Content length is %ld bytes", total_length)
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
            path_tmp.rename(path)
            break
        except Exception:
            if sys.stdout.isatty():
                sys.stdout.write("\n")
            if i + 1 < DOWNLOAD_RETRIES_COUNT:
                time.sleep(3)

            path.unlink(True)
    else:
        raise Exception(f"Cannot download dataset from {url}, all retries exceeded")

    if sys.stdout.isatty():
        sys.stdout.write("\n")
    logging.info("Downloading finished")
