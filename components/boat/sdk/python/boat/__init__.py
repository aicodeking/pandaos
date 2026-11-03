"""Boat Python SDK (stdlib only). See README.md."""
from .client import (
    Boat,
    BoatError,
    Command,
    DesktopURL,
    ExecEvent,
    ExecResult,
    JobResult,
    PromptRun,
    SSHInfo,
    VM,
    verify_webhook,
)

__all__ = ["Boat", "BoatError", "Command", "DesktopURL", "ExecEvent", "ExecResult",
    "JobResult", "PromptRun", "SSHInfo", "VM",
           "verify_webhook"]
__version__ = "0.1.0"
