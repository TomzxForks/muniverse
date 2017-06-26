"""
API for controlling muniverse environments.
"""

from .handle import Handle

class Env:
    """
    An environment instance.
    """
    def __init__(self, spec, container=None, chrome_host=None, game_host=None):
        """
        Create a new environment from the specification.

        If container is set, launch the environment in a
        custom container.

        If chrome_host and game_host are set, launch the
        environment in a running chrome instance.

        This may raise an exception if a muniverse-bind
        process cannot be started.
        It also raises a MuniverseError if the environment
        cannot be created on the backend.
        """
        if (chrome_host is None) != (game_host is None):
            raise ValueError('must set both chrome_host and game_host')
        elif (not container is None) and (not chrome_host is None):
            raise ValueError('cannot mix chrome_host and container options')
        call_obj = {'Spec': spec}
        call_name = 'NewEnv'
        if not container is None:
            call_obj['Container'] = container
            call_name = 'NewEnvContainer'
        elif not chrome_host is None:
            call_obj['Host'] = chrome_host
            call_obj['GameHost'] = game_host
            call_name = 'NewEnvChrome'
        self.handle = None
        handle = Handle()
        try:
            self.uid = handle.checked_call(call_name, call_obj)['UID']
            self.handle = handle
        finally:
            if self.handle is None:
                handle.close()

    def reset(self):
        """
        Reset the environment to a starting state.
        """
        self.handle.checked_call('Reset', {'UID': self.uid})

    def close(self):
        """
        Stop and clean up the environment.

        You should not close an environment multiple times.
        """
        try:
            self.handle.checked_call('CloseEnv', {'UID': self.uid})
        finally:
            self.handle.close()
