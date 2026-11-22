"""Boat Python SDK (stdlib only). See README.md."""
from .client import (
    Boat,
    BoatError,
    BoatTimeoutError,
    Command,
    DesktopURL,
    ExecEvent,
    ExecResult,
    JobResult,
    PromptRun,
    SSHInfo,
    VM,
    VMHandle,
    verify_webhook,
)

__all__ = ["Boat", "BoatError", "BoatTimeoutError", "Command", "DesktopURL", "ExecEvent", "ExecResult",
    "JobResult", "PromptRun", "SSHInfo", "VM", "VMHandle",
           "verify_webhook"]
__version__ = "0.1.0"
