"""Fixture application."""

import os


def build_tool(name):
    """Build a tool named ``name``."""
    return name


class Runner:
    """Runs tools."""

    def run(self):
        return build_tool("x")
