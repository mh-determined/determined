import inspect
from typing import Any, Dict, List, Sequence, Tuple, Union, cast

import pytorch_lightning as pl
import torch
from pytorch_lightning.trainer.optimizers import TrainerOptimizersMixin
from torch.optim.lr_scheduler import _LRScheduler
from torch.optim.optimizer import Optimizer

from determined.errors import InvalidModelException
from determined.monkey_patch import monkey_patch
from determined.pytorch import LRScheduler, PyTorchCallback, PyTorchTrial, PyTorchTrialContext
from determined.util import has_param, is_overridden
from determined_common import check

TorchData = Union[Dict[str, torch.Tensor], Sequence[torch.Tensor], torch.Tensor]


def check_compatibility(lm: pl.LightningModule) -> None:
    prefix = "Unsupported usage in PLAdapter: "
    unsupported_members = {"training_step_end", "validation_step_end", "manual_backward"}

    members = inspect.getmembers(lm, predicate=inspect.ismethod)
    overridden_members = set(
        map(lambda m: m[0], filter(lambda m: is_overridden(m[0], lm), members))
    )

    matches = unsupported_members & overridden_members
    if len(matches) > 0:
        raise InvalidModelException(prefix + f"{matches}")

    for member in overridden_members:
        if has_param(getattr(lm, member), "dataloader_idx"):
            raise InvalidModelException(
                prefix
                + f'multiple dataloaders and `dataloader_idx` are not supported in "{member}"'
            )

    if has_param(lm.training_step, "hiddens", 4):
        raise InvalidModelException(prefix + '`hiddens` argument in "training_step"')

    if lm.trainer is not None:
        raise InvalidModelException(prefix + "Lightning Trainer")


class PLAdapter(PyTorchTrial):
    context: PyTorchTrialContext
    lm: pl.LightningModule

    def __init__(self, context: PyTorchTrialContext, lightning_module: pl.LightningModule):
        super().__init__(context)
        check_compatibility(lightning_module)
        context.wrap_model(lightning_module)
        self.lm = lightning_module
        self.context = context

        optimizers, lr_schedulers = self.setup_optimizers_schedulers(context)
        self.optimizers = optimizers

        # set lightning_module properties
        self.lm.use_ddp = False
        self.lm.use_ddp2 = False
        self.lm.use_dp = False
        self.lm.use_tpu = False
        type(self.lm).local_rank = self.context.distributed.get_local_rank() # type: ignore
        type(self.lm).global_rank = self.context.distributed.get_rank() # type: ignore
        self.lm.use_amp = self.context._use_amp
        self.lm.to(self.context.device)

    def build_callbacks(self) -> Dict[str, PyTorchCallback]:
        """
        build_callbacks defines a set of necessary PyTorchTrialCallback to support
        lightning. Override and merge the output of this build_callbacks with your
        desired callbacks.
        """
        context = self.context
        lm = self.lm
        class PLAdapterCallback(PyTorchCallback):
            def on_train_epoch_end(self, output: List[Any]) -> None:
                lm.on_train_epoch_end(output)
                lm.training_epoch_end(output)

            def on_validation_epoch_end(self, outputs: List[Any]) -> None:
                lm.on_validation_epoch_end()
                lm.validation_epoch_end(outputs)

            def on_train_epoch_start(self) -> None:
                if context._current_batch_idx is not None:
                    type(lm).current_epoch = context.current_train_epoch() # type: ignore
                lm.on_train_epoch_start()

            def on_validation_epoch_start(self) -> None:
                lm.on_validation_epoch_start()

        return {"_lightning_module": PLAdapterCallback()}

    def setup_optimizers_schedulers(
        self,
        context: PyTorchTrialContext,
    ) -> Tuple[List[Optimizer], List[_LRScheduler]]:
        """
        Wrap optimizers and lr_schedulers returned by `configure_optimizers` to
        work with Determined.
        Return: Wrapped `optimizers`, and `lr_schedulers` in a tuple
        """
        optimizers, lr_scheduler_dicts, opt_frequencies = TrainerOptimizersMixin().init_optimizers(
            self.lm
        )
        for freq in opt_frequencies:
            check.eq(freq, 1, "custom optimizer frequencies are not supported")
        optimizers = cast(List[Optimizer], optimizers)
        lr_scheduler_dicts = cast(List[dict], lr_scheduler_dicts)

        def lightning_scheduler_dict_to_det(lrs: dict) -> _LRScheduler:
            """
            input_dict = {
                'scheduler': None,
                'name': None,  # no custom name
                'interval': 'epoch',  # after epoch is over
                'frequency': 1,  # every epoch/batch
                'reduce_on_plateau': False,  # most often not ReduceLROnPlateau scheduler
                'monitor': monitor,  # value to monitor for ReduceLROnPlateau
                'strict': True,  # enforce that the monitor exists for ReduceLROnPlateau
            }
            """
            # TODO(DET-5021) support custom frequencies with the manual step.
            step_mode = (
                LRScheduler.StepMode.STEP_EVERY_EPOCH
                if lrs["interval"] == "epoch"
                else LRScheduler.StepMode.STEP_EVERY_BATCH
            )
            return context.wrap_lr_scheduler(lrs["scheduler"], step_mode)

        optimizers = [self.context.wrap_optimizer(opt) for opt in optimizers]
        lr_schedulers = [lightning_scheduler_dict_to_det(lrs) for lrs in lr_scheduler_dicts]
        return optimizers, lr_schedulers

    def _build_train_args(self, batch, batch_idx, opt_idx) -> List[Any]:
        # taken from pytorch_lightning
        args = [batch, batch_idx]

        if len(self.optimizers) > 1:
            if has_param(self.lm.training_step, "optimizer_idx"):
                args.append(opt_idx)
            else:
                num_opts = len(self.optimizers)
                raise InvalidModelException(
                    f"Your LightningModule defines {num_opts} optimizers but "
                    f'training_step is missing the "optimizer_idx" argument.'
                )

        return args

    def train_batch(
        self, batch: TorchData, epoch_idx: int, batch_idx: int
    ) -> Union[torch.Tensor, Dict[str, Any]]:
        type(self.lm).global_step = batch_idx # type: ignore
        self.lm.on_train_batch_start(batch, batch_idx, dataloader_idx=0)

        opt_metrics = []
        metrics = None

        for opt_idx, opt in enumerate(self.optimizers):
            with monkey_patch(self.lm, "optimizers", lambda *args, **kwargs: self.optimizers):
                self.lm.toggle_optimizer(opt, opt_idx)
            train_args = self._build_train_args(batch, batch_idx, opt_idx)
            metrics = self.lm.training_step(*train_args)

            if metrics is None:
                continue
            elif not isinstance(metrics, dict):
                metrics = {"loss": metrics}

            self.context.backward(metrics["loss"])
            self.lm.on_after_backward()
            self.context.step_optimizer(opt, on_before_zero_grad=self.lm.on_before_zero_grad)

            opt_metrics.append(metrics)
            with monkey_patch(self.lm, "optimizers", lambda *args, **kwargs: self.optimizers):
                self.lm.untoggle_optimizer(opt_idx)

        self.lm.on_train_batch_end(metrics, batch, batch_idx, dataloader_idx=0)

        agg_metrics = {}
        for opt_idx, rv in enumerate(opt_metrics):
            for k, v in rv.items():
                agg_metrics[f"opt{opt_idx}_{k}"] = v
        return agg_metrics

    def evaluate_batch(self, batch: TorchData, batch_idx: int) -> Dict[str, Any]:
        self.lm.on_validation_batch_start(batch, batch_idx, dataloader_idx=0)
        rv = self.lm.validation_step(batch, batch_idx=batch_idx)
        self.lm.on_validation_batch_end(rv, batch, batch_idx, dataloader_idx=0)

        metrics = None
        if rv is None:
            metrics = {}
        elif not isinstance(rv, dict):
            metrics = {"loss": rv}
        else:
            metrics = rv
        return metrics