from pathlib import Path
from typing import Optional, Union
import logging
import sys
import time

import requests  # type: ignore

DOWNLOAD_RETRIES_COUNT = 3

# Used by default if not set for functions
logger = logging.getLogger(__name__)


class DownloadException(BaseException):
    pass


def get_with_retries(
    url: str,
    retries: int = DOWNLOAD_RETRIES_COUNT,
    sleep: int = 3,
    logger: logging.Logger = logger,
    **kwargs,
) -> requests.Response:
    logger.info(
        "Getting URL with %i tries and sleep %i in between: %s", retries, sleep, url
    )
    exc = None  # type: Optional[Union[Exception, BaseException]]
    for i in range(retries):
        try:
            timeout = kwargs.pop("timeout", 20)
            response = requests.get(url, timeout=timeout, **kwargs)
            response.raise_for_status()
            break
        except (BaseException, Exception) as e:
            if i + 1 < retries:
                logger.info("Exception '%s' while getting, retry %i", e, i + 1)
                time.sleep(sleep)

            exc = e
    else:
        raise DownloadException(exc)

    return response


def download(
    url: str, path: Path, with_progress: bool = False, logger: logging.Logger = logger
):
    logger.info("Downloading from %s to path %s", url, path)
    with_progress = with_progress and sys.stdout.isatty()
    path_tmp = path.with_suffix(".tmp")
    for i in range(DOWNLOAD_RETRIES_COUNT):
        try:
            with open(path_tmp, "wb") as f:
                response = get_with_retries(url, retries=1, stream=True, logger=logger)
                total_length = response.headers.get("content-length")
                if total_length is None or int(total_length) == 0:
                    logger.info(
                        "No content-length, will download file without progress"
                    )
                    f.write(response.content)
                else:
                    dl = 0
                    total_length = int(total_length)
                    logger.info("Content length is %ld bytes", total_length)
                    for data in response.iter_content(chunk_size=4096):
                        f.write(data)
                        if with_progress:
                            dl += len(data)
                            done = int(50 * dl / total_length)
                            percent = int(100 * float(dl) / total_length)
                            eq_str = "=" * done
                            space_str = " " * (50 - done)
                            sys.stdout.write(f"\r[{eq_str}{space_str}] {percent}%")
                            sys.stdout.flush()
            path_tmp.rename(path)
            break
        except (BaseException, Exception) as e:
            if with_progress:
                sys.stdout.write("\n")
            if i + 1 < DOWNLOAD_RETRIES_COUNT:
                time.sleep(3)

            path.unlink(True)
    else:
        raise DownloadException(
            f"Cannot download dataset from {url}, all retries exceeded"
        )

    if with_progress:
        sys.stdout.write("\n")
    logger.info("Downloading finished")
