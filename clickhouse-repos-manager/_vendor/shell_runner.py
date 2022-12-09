#!/usr/bin/env python
from typing import Any, Optional
import logging
import os.path as p
import subprocess

logger = logging.getLogger(__name__)

CWD = p.dirname(p.realpath(__file__))


class Runner:
    """lightweight check_output wrapper with stripping last NEW_LINE"""

    def __init__(self, cwd: str = CWD):
        self._cwd = cwd

    def run(self, cmd: str, cwd: Optional[str] = None, **kwargs: Any) -> str:
        if cwd is None:
            cwd = self.cwd
        logger.debug("Running command: %s", cmd)
        output = str(
            subprocess.check_output(
                cmd, shell=True, cwd=cwd, encoding="utf-8", **kwargs
            ).strip()
        )
        return output

    @property
    def cwd(self) -> str:
        return self._cwd

    @cwd.setter
    def cwd(self, value: str) -> None:
        # Set _cwd only once, then set it to readonly
        if self._cwd != CWD:
            return
        self._cwd = value

    def __call__(self, *args, **kwargs):
        return self.run(*args, **kwargs)
